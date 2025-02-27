package resources

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"fmt"
	"log"
	"strings"

	"github.com/Snowflake-Labs/terraform-provider-snowflake/pkg/snowflake"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/pkg/errors"
	"golang.org/x/exp/slices"
)

const (
	tableIDDelimiter = '|'
)

var tableSchema = map[string]*schema.Schema{
	"name": {
		Type:        schema.TypeString,
		Required:    true,
		Description: "Specifies the identifier for the table; must be unique for the database and schema in which the table is created.",
	},
	"schema": {
		Type:        schema.TypeString,
		Required:    true,
		ForceNew:    true,
		Description: "The schema in which to create the table.",
	},
	"database": {
		Type:        schema.TypeString,
		Required:    true,
		ForceNew:    true,
		Description: "The database in which to create the table.",
	},
	"cluster_by": {
		Type:        schema.TypeList,
		Elem:        &schema.Schema{Type: schema.TypeString},
		Optional:    true,
		Description: "A list of one or more table columns/expressions to be used as clustering key(s) for the table",
	},
	"column": {
		Type:        schema.TypeList,
		Required:    true,
		MinItems:    1,
		Description: "Definitions of a column to create in the table. Minimum one required.",
		Elem: &schema.Resource{
			Schema: map[string]*schema.Schema{
				"name": {
					Type:        schema.TypeString,
					Required:    true,
					Description: "Column name",
				},
				"type": {
					Type:        schema.TypeString,
					Required:    true,
					Description: "Column type, e.g. VARIANT",
					DiffSuppressFunc: func(k, old, new string, d *schema.ResourceData) bool {
						// these are all equivalent as per https://docs.snowflake.com/en/sql-reference/data-types-text.html
						varcharType := []string{"VARCHAR(16777216)", "VARCHAR", "text", "string", "NVARCHAR", "NVARCHAR2", "CHAR VARYING", "NCHAR VARYING"}
						return slices.Contains(varcharType, new) && slices.Contains(varcharType, old)
					},
				},
				"nullable": {
					Type:        schema.TypeBool,
					Optional:    true,
					Default:     true,
					Description: "Whether this column can contain null values. **Note**: Depending on your Snowflake version, the default value will not suffice if this column is used in a primary key constraint.",
				},
				"default": {
					Type:        schema.TypeList,
					Optional:    true,
					Description: "Defines the column default value; note due to limitations of Snowflake's ALTER TABLE ADD/MODIFY COLUMN updates to default will not be applied",
					MinItems:    1,
					MaxItems:    1,
					Elem: &schema.Resource{
						Schema: map[string]*schema.Schema{
							"constant": {
								Type:        schema.TypeString,
								Optional:    true,
								Description: "The default constant value for the column",
								// ConflictsWith: []string{".expression", ".sequence"}, - can't use, nor ExactlyOneOf due to column type being TypeList
							},
							"expression": {
								Type:        schema.TypeString,
								Optional:    true,
								Description: "The default expression value for the column",
								// ConflictsWith: []string{".constant", ".sequence"}, - can't use, nor ExactlyOneOf due to column type being TypeList
							},
							"sequence": {
								Type:        schema.TypeString,
								Optional:    true,
								Description: "The default sequence to use for the column",
								// ConflictsWith: []string{".constant", ".expression"}, - can't use, nor ExactlyOneOf due to column type being TypeList
							},
						},
					},
				},
				/*Note: Identity and default are mutually exclusive. From what I can tell we can't enforce this here
				the snowflake query will error so we can defer enforcement to there.
				*/
				"identity": {
					Type:        schema.TypeList,
					Optional:    true,
					Description: "Defines the identity start/step values for a column. **Note** Identity/default are mutually exclusive.",
					MinItems:    1,
					MaxItems:    1,
					Elem: &schema.Resource{
						Schema: map[string]*schema.Schema{
							"start_num": {
								Type:        schema.TypeInt,
								Optional:    true,
								Description: "The number to start incrementing at.",
								Default:     1,
							},
							"step_num": {
								Type:        schema.TypeInt,
								Optional:    true,
								Description: "Step size to increment by.",
								Default:     1,
							},
						},
					},
				},
				"comment": {
					Type:        schema.TypeString,
					Optional:    true,
					Default:     "",
					Description: "Column comment",
				},
				"masking_policy": {
					Type:        schema.TypeString,
					Optional:    true,
					Default:     "",
					Description: "Masking policy to apply on column",
				},
			},
		},
	},
	"comment": {
		Type:        schema.TypeString,
		Optional:    true,
		Description: "Specifies a comment for the table.",
	},
	"owner": {
		Type:        schema.TypeString,
		Computed:    true,
		Description: "Name of the role that owns the table.",
	},
	"primary_key": {
		Type:        schema.TypeList,
		Optional:    true,
		MaxItems:    1,
		Description: "Definitions of primary key constraint to create on table",
		Deprecated:  "Use snowflake_table_constraint instead",
		Elem: &schema.Resource{
			Schema: map[string]*schema.Schema{
				"name": {
					Type:        schema.TypeString,
					Optional:    true,
					Description: "Name of constraint",
				},
				"keys": {
					Type: schema.TypeList,
					Elem: &schema.Schema{
						Type: schema.TypeString,
					},
					Required:    true,
					Description: "Columns to use in primary key",
				},
			},
		},
	},
	"data_retention_days": {
		Type:         schema.TypeInt,
		Optional:     true,
		Default:      1,
		Description:  "Specifies the retention period for the table so that Time Travel actions (SELECT, CLONE, UNDROP) can be performed on historical data in the table. Default value is 1, if you wish to inherit the parent schema setting then pass in the schema attribute to this argument.",
		ValidateFunc: validation.IntBetween(0, 90),
	},
	"change_tracking": {
		Type:        schema.TypeBool,
		Optional:    true,
		Default:     false,
		Description: "Specifies whether to enable change tracking on the table. Default false.",
	},
	"tag": tagReferenceSchema,
}

func Table() *schema.Resource {
	return &schema.Resource{
		Create: CreateTable,
		Read:   ReadTable,
		Update: UpdateTable,
		Delete: DeleteTable,

		Schema: tableSchema,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
	}
}

type tableID struct {
	DatabaseName string
	SchemaName   string
	TableName    string
}

// String() takes in a tableID object and returns a pipe-delimited string:
// DatabaseName|SchemaName|TableName.
func (si *tableID) String() (string, error) {
	var buf bytes.Buffer
	csvWriter := csv.NewWriter(&buf)
	csvWriter.Comma = tableIDDelimiter
	dataIdentifiers := [][]string{{si.DatabaseName, si.SchemaName, si.TableName}}
	err := csvWriter.WriteAll(dataIdentifiers)
	if err != nil {
		return "", err
	}
	strTableID := strings.TrimSpace(buf.String())
	return strTableID, nil
}

// tableIDFromString() takes in a pipe-delimited string: DatabaseName|SchemaName|TableName
// and returns a tableID object.
func tableIDFromString(stringID string) (*tableID, error) {
	reader := csv.NewReader(strings.NewReader(stringID))
	reader.Comma = tableIDDelimiter
	lines, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("not CSV compatible")
	}

	if len(lines) != 1 {
		return nil, fmt.Errorf("1 line at a time")
	}
	if len(lines[0]) != 3 {
		return nil, fmt.Errorf("3 fields allowed")
	}

	tableResult := &tableID{
		DatabaseName: lines[0][0],
		SchemaName:   lines[0][1],
		TableName:    lines[0][2],
	}
	return tableResult, nil
}

type columnDefault struct {
	constant   *string
	expression *string
	sequence   *string
}

func (cd *columnDefault) toSnowflakeColumnDefault() *snowflake.ColumnDefault {
	if cd.constant != nil {
		return snowflake.NewColumnDefaultWithConstant(*cd.constant)
	}

	if cd.expression != nil {
		return snowflake.NewColumnDefaultWithExpression(*cd.expression)
	}

	if cd.sequence != nil {
		return snowflake.NewColumnDefaultWithSequence(*cd.sequence)
	}

	return nil
}

func (cd *columnDefault) _type() string {
	if cd.constant != nil {
		return "constant"
	}

	if cd.expression != nil {
		return "expression"
	}

	if cd.sequence != nil {
		return "sequence"
	}

	return "unknown"
}

type columnIdentity struct {
	startNum int
	stepNum  int
}

func (identity *columnIdentity) toSnowflakeColumnIdentity() *snowflake.ColumnIdentity {
	snowIdentity := snowflake.ColumnIdentity{}
	return snowIdentity.WithStartNum(identity.startNum).WithStep(identity.stepNum)
}

type column struct {
	name          string
	dataType      string
	nullable      bool
	_default      *columnDefault
	identity      *columnIdentity
	comment       string
	maskingPolicy string
}

func (c column) toSnowflakeColumn() snowflake.Column {
	sC := &snowflake.Column{}

	if c._default != nil {
		sC = sC.WithDefault(c._default.toSnowflakeColumnDefault())
	}

	if c.identity != nil {
		sC = sC.WithIdentity(c.identity.toSnowflakeColumnIdentity())
	}

	return *sC.WithName(c.name).
		WithType(c.dataType).
		WithNullable(c.nullable).
		WithComment(c.comment).
		WithMaskingPolicy(c.maskingPolicy)
}

type columns []column

func (c columns) toSnowflakeColumns() []snowflake.Column {
	sC := make([]snowflake.Column, len(c))
	for i, col := range c {
		sC[i] = col.toSnowflakeColumn()
	}
	return sC
}

type changedColumns []changedColumn

type changedColumn struct {
	newColumn             column //our new column
	changedDataType       bool
	changedNullConstraint bool
	dropedDefault         bool
	changedComment        bool
	changedMaskingPolicy  bool
}

func (old columns) getChangedColumnProperties(new columns) (changed changedColumns) {
	changed = changedColumns{}
	for _, cO := range old {
		for _, cN := range new {
			changeColumn := changedColumn{cN, false, false, false, false, false}
			if cO.name == cN.name && cO.dataType != cN.dataType {
				changeColumn.changedDataType = true
			}
			if cO.name == cN.name && cO.nullable != cN.nullable {
				changeColumn.changedNullConstraint = true
			}
			if cO.name == cN.name && cO._default != nil && cN._default == nil {
				changeColumn.dropedDefault = true
			}

			if cO.name == cN.name && cO.comment != cN.comment {
				changeColumn.changedComment = true
			}

			if cO.name == cN.name && cO.maskingPolicy != cN.maskingPolicy {
				changeColumn.changedMaskingPolicy = true
			}

			changed = append(changed, changeColumn)
		}
	}
	return
}

func (old columns) diffs(new columns) (removed columns, added columns, changed changedColumns) {
	return old.getNewIn(new), new.getNewIn(old), old.getChangedColumnProperties(new)
}

func getColumnDefault(def map[string]interface{}) *columnDefault {
	if c, ok := def["constant"]; ok {
		if constant, ok := c.(string); ok && len(constant) > 0 {
			return &columnDefault{
				constant: &constant,
			}
		}
	}

	if e, ok := def["expression"]; ok {
		if expr, ok := e.(string); ok && len(expr) > 0 {
			return &columnDefault{
				expression: &expr,
			}
		}
	}

	if s, ok := def["sequence"]; ok {
		if seq := s.(string); ok && len(seq) > 0 {
			return &columnDefault{
				sequence: &seq,
			}
		}
	}

	return nil
}

func getColumnIdentity(identity map[string]interface{}) *columnIdentity {
	if len(identity) > 0 {
		startNum := identity["start_num"].(int)
		stepNum := identity["step_num"].(int)
		return &columnIdentity{startNum, stepNum}
	}

	return nil
}

func getColumn(from interface{}) (to column) {
	c := from.(map[string]interface{})
	var cd *columnDefault
	var id *columnIdentity

	_default := c["default"].([]interface{})
	identity := c["identity"].([]interface{})

	if len(_default) == 1 {
		cd = getColumnDefault(_default[0].(map[string]interface{}))
	}
	if len(identity) == 1 {
		id = getColumnIdentity(identity[0].(map[string]interface{}))
	}

	return column{
		name:          c["name"].(string),
		dataType:      c["type"].(string),
		nullable:      c["nullable"].(bool),
		_default:      cd,
		identity:      id,
		comment:       c["comment"].(string),
		maskingPolicy: c["masking_policy"].(string),
	}
}

func getColumns(from interface{}) (to columns) {
	cols := from.([]interface{})
	to = make(columns, len(cols))
	for i, c := range cols {
		to[i] = getColumn(c)
	}
	return to
}

type primarykey struct {
	name string
	keys []string
}

func getPrimaryKey(from interface{}) (to primarykey) {
	pk := from.([]interface{})
	to = primarykey{}
	if len(pk) > 0 {
		pkDetails := pk[0].(map[string]interface{})
		to.name = pkDetails["name"].(string)
		to.keys = expandStringList(pkDetails["keys"].([]interface{}))
		return to
	}
	return to
}

func (pk primarykey) toSnowflakePrimaryKey() snowflake.PrimaryKey {
	snowPk := snowflake.PrimaryKey{}
	return *snowPk.WithName(pk.name).WithKeys(pk.keys)
}

// CreateTable implements schema.CreateFunc.
func CreateTable(d *schema.ResourceData, meta interface{}) error {
	db := meta.(*sql.DB)
	database := d.Get("database").(string)
	schema := d.Get("schema").(string)
	name := d.Get("name").(string)

	columns := getColumns(d.Get("column").([]interface{}))

	builder := snowflake.TableWithColumnDefinitions(name, database, schema, columns.toSnowflakeColumns())

	// Set optionals
	if v, ok := d.GetOk("comment"); ok {
		builder.WithComment(v.(string))
	}

	if v, ok := d.GetOk("cluster_by"); ok {
		builder.WithClustering(expandStringList(v.([]interface{})))
	}

	if v, ok := d.GetOk("primary_key"); ok {
		pk := getPrimaryKey(v.([]interface{}))
		builder.WithPrimaryKey(pk.toSnowflakePrimaryKey())
	}

	if v, ok := d.GetOk("data_retention_days"); ok {
		builder.WithDataRetentionTimeInDays(v.(int))
	}

	if v, ok := d.GetOk("change_tracking"); ok {
		builder.WithChangeTracking(v.(bool))
	}

	if v, ok := d.GetOk("tag"); ok {
		tags := getTags(v)
		builder.WithTags(tags.toSnowflakeTagValues())
	}

	stmt := builder.Create()
	err := snowflake.Exec(db, stmt)
	if err != nil {
		return errors.Wrapf(err, "error creating table %v", name)
	}

	tableID := &tableID{
		DatabaseName: database,
		SchemaName:   schema,
		TableName:    name,
	}
	dataIDInput, err := tableID.String()
	if err != nil {
		return err
	}
	d.SetId(dataIDInput)

	return ReadTable(d, meta)
}

// ReadTable implements schema.ReadFunc.
func ReadTable(d *schema.ResourceData, meta interface{}) error {
	db := meta.(*sql.DB)
	tableID, err := tableIDFromString(d.Id())
	if err != nil {
		return err
	}
	builder := snowflake.Table(tableID.TableName, tableID.DatabaseName, tableID.SchemaName)

	row := snowflake.QueryRow(db, builder.Show())
	table, err := snowflake.ScanTable(row)
	if err == sql.ErrNoRows {
		// If not found, mark resource to be removed from statefile during apply or refresh
		log.Printf("[DEBUG] table (%s) not found", d.Id())
		d.SetId("")
		return nil
	}
	if err != nil {
		return err
	}

	// Describe the table to read the cols
	tableDescriptionRows, err := snowflake.Query(db, builder.ShowColumns())
	if err != nil {
		return err
	}

	tableDescription, err := snowflake.ScanTableDescription(tableDescriptionRows)
	if err != nil {
		return err
	}

	/*
		deprecated as it conflicts with the new table_constraint resource
		showPkrows, err := snowflake.Query(db, builder.ShowPrimaryKeys())
		if err != nil {
			return err
		}

		pkDescription, err := snowflake.ScanPrimaryKeyDescription(showPkrows)
		if err != nil {
			return err
		}*/

	// Set the relevant data in the state
	toSet := map[string]interface{}{
		"name":       table.TableName.String,
		"owner":      table.Owner.String,
		"database":   tableID.DatabaseName,
		"schema":     tableID.SchemaName,
		"comment":    table.Comment.String,
		"column":     snowflake.NewColumns(tableDescription).Flatten(),
		"cluster_by": snowflake.ClusterStatementToList(table.ClusterBy.String),
		//"primary_key":         snowflake.FlattenTablePrimaryKey(pkDescription),
		"data_retention_days": table.RetentionTime.Int32,
		"change_tracking":     (table.ChangeTracking.String == "ON"),
	}

	for key, val := range toSet {
		err = d.Set(key, val) //lintignore:R001
		if err != nil {
			return err
		}
	}
	return nil
}

// UpdateTable implements schema.UpdateFunc.
func UpdateTable(d *schema.ResourceData, meta interface{}) error {
	tableID, err := tableIDFromString(d.Id())
	if err != nil {
		return err
	}

	dbName := tableID.DatabaseName
	schema := tableID.SchemaName
	tableName := tableID.TableName

	builder := snowflake.Table(tableName, dbName, schema)

	db := meta.(*sql.DB)
	if d.HasChange("name") {
		name := d.Get("name")
		q := builder.Rename(name.(string))
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating table name on %v", d.Id())
		}
		d.SetId(fmt.Sprintf("%v|%v|%v", dbName, schema, name.(string)))
	}
	if d.HasChange("comment") {
		comment := d.Get("comment")
		q := builder.ChangeComment(comment.(string))
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating table comment on %v", d.Id())
		}
	}
	if d.HasChange("cluster_by") {
		cb := expandStringList(d.Get("cluster_by").([]interface{}))

		var q string
		if len(cb) != 0 {
			builder.WithClustering(cb)
			q = builder.ChangeClusterBy(builder.GetClusterKeyString())
		} else {
			q = builder.DropClustering()
		}

		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating table clustering on %v", d.Id())
		}
	}
	if d.HasChange("column") {
		old, new := d.GetChange("column")
		removed, added, changed := getColumns(old).diffs(getColumns(new))
		for _, cA := range removed {
			q := builder.DropColumn(cA.name)
			err := snowflake.Exec(db, q)
			if err != nil {
				return errors.Wrapf(err, "error dropping column on %v", d.Id())
			}
		}
		for _, cA := range added {
			var q string

			if cA.identity == nil && cA._default == nil {
				q = builder.AddColumn(cA.name, cA.dataType, cA.nullable, nil, nil, cA.comment, cA.maskingPolicy)
			} else if cA.identity != nil {
				q = builder.AddColumn(cA.name, cA.dataType, cA.nullable, nil, cA.identity.toSnowflakeColumnIdentity(), cA.comment, cA.maskingPolicy)
			} else {
				if cA._default._type() != "constant" {
					return fmt.Errorf("failed to add column %v => Only adding a column as a constant is supported by Snowflake", cA.name)
				}

				q = builder.AddColumn(cA.name, cA.dataType, cA.nullable, cA._default.toSnowflakeColumnDefault(), nil, cA.comment, cA.maskingPolicy)
			}

			err := snowflake.Exec(db, q)
			if err != nil {
				return errors.Wrapf(err, "error adding column on %v", d.Id())
			}
		}
		for _, cA := range changed {
			if cA.changedDataType {
				q := builder.ChangeColumnType(cA.newColumn.name, cA.newColumn.dataType)
				err := snowflake.Exec(db, q)
				if err != nil {
					return errors.Wrapf(err, "error changing property on %v", d.Id())
				}
			}
			if cA.changedNullConstraint {
				q := builder.ChangeNullConstraint(cA.newColumn.name, cA.newColumn.nullable)
				err := snowflake.Exec(db, q)
				if err != nil {
					return errors.Wrapf(err, "error changing property on %v", d.Id())
				}
			}
			if cA.dropedDefault {
				q := builder.DropColumnDefault(cA.newColumn.name)
				err := snowflake.Exec(db, q)
				if err != nil {
					return errors.Wrapf(err, "error changing property on %v", d.Id())
				}
			}
			if cA.changedComment {
				q := builder.ChangeColumnComment(cA.newColumn.name, cA.newColumn.comment)
				err := snowflake.Exec(db, q)
				if err != nil {
					return errors.Wrapf(err, "error changing property on %v", d.Id())
				}
			}
			if cA.changedMaskingPolicy {
				q := builder.ChangeColumnMaskingPolicy(cA.newColumn.name, cA.newColumn.maskingPolicy)
				err := snowflake.Exec(db, q)
				if err != nil {
					return errors.Wrapf(err, "error changing property on %v", d.Id())
				}
			}
		}
	}
	if d.HasChange("primary_key") {
		opk, npk := d.GetChange("primary_key")

		newpk := getPrimaryKey(npk)
		oldpk := getPrimaryKey(opk)

		if len(oldpk.keys) > 0 || len(newpk.keys) == 0 {
			//drop our pk if there was an old primary key, or pk has been removed
			q := builder.DropPrimaryKey()
			err := snowflake.Exec(db, q)
			if err != nil {
				return errors.Wrapf(err, "error changing primary key first on %v", d.Id())
			}
		}

		if len(newpk.keys) > 0 {
			// add our new pk
			q := builder.ChangePrimaryKey(newpk.toSnowflakePrimaryKey())
			err := snowflake.Exec(db, q)
			if err != nil {
				return errors.Wrapf(err, "error changing property on %v", d.Id())
			}
		}
	}
	if d.HasChange("data_retention_days") {
		ndr := d.Get("data_retention_days")

		q := builder.ChangeDataRetention(ndr.(int))
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error changing property on %v", d.Id())
		}
	}
	if d.HasChange("change_tracking") {
		nct := d.Get("change_tracking")

		q := builder.ChangeChangeTracking(nct.(bool))
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error changing property on %v", d.Id())
		}
	}
	tagChangeErr := handleTagChanges(db, d, builder)
	if tagChangeErr != nil {
		return tagChangeErr
	}

	return ReadTable(d, meta)
}

// DeleteTable implements schema.DeleteFunc.
func DeleteTable(d *schema.ResourceData, meta interface{}) error {
	db := meta.(*sql.DB)
	tableID, err := tableIDFromString(d.Id())
	if err != nil {
		return err
	}

	dbName := tableID.DatabaseName
	schemaName := tableID.SchemaName
	tableName := tableID.TableName

	q := snowflake.Table(tableName, dbName, schemaName).Drop()

	err = snowflake.Exec(db, q)
	if err != nil {
		return errors.Wrapf(err, "error deleting pipe %v", d.Id())
	}

	d.SetId("")

	return nil
}
