package resources

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/Snowflake-Labs/terraform-provider-snowflake/pkg/snowflake"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/pkg/errors"
	"golang.org/x/exp/slices"
)

const (
	taskIDDelimiter = '|'
)

var taskSchema = map[string]*schema.Schema{
	"enabled": {
		Type:        schema.TypeBool,
		Optional:    true,
		Default:     false,
		Description: "Specifies if the task should be started (enabled) after creation or should remain suspended (default).",
	},
	"name": {
		Type:        schema.TypeString,
		Required:    true,
		Description: "Specifies the identifier for the task; must be unique for the database and schema in which the task is created.",
		ForceNew:    true,
	},
	"database": {
		Type:        schema.TypeString,
		Required:    true,
		Description: "The database in which to create the task.",
		ForceNew:    true,
	},
	"schema": {
		Type:        schema.TypeString,
		Required:    true,
		Description: "The schema in which to create the task.",
		ForceNew:    true,
	},
	"warehouse": {
		Type:          schema.TypeString,
		Optional:      true,
		Description:   "The warehouse the task will use. Omit this parameter to use Snowflake-managed compute resources for runs of this task. (Conflicts with user_task_managed_initial_warehouse_size)",
		ForceNew:      false,
		ConflictsWith: []string{"user_task_managed_initial_warehouse_size"},
	},
	"schedule": {
		Type:          schema.TypeString,
		Optional:      true,
		Description:   "The schedule for periodically running the task. This can be a cron or interval in minutes. (Conflict with after)",
		ConflictsWith: []string{"after"},
	},
	"session_parameters": {
		Type:        schema.TypeMap,
		Elem:        &schema.Schema{Type: schema.TypeString},
		Optional:    true,
		Description: "Specifies session parameters to set for the session when the task runs. A task supports all session parameters.",
	},
	"user_task_timeout_ms": {
		Type:         schema.TypeInt,
		Optional:     true,
		ValidateFunc: validation.IntBetween(0, 86400000),
		Description:  "Specifies the time limit on a single run of the task before it times out (in milliseconds).",
	},
	"comment": {
		Type:        schema.TypeString,
		Optional:    true,
		Description: "Specifies a comment for the task.",
	},
	"after": {
		Type:          schema.TypeList,
		Elem:          &schema.Schema{Type: schema.TypeString},
		Optional:      true,
		Description:   "Specifies one or more predecessor tasks for the current task. Use this option to create a DAG of tasks or add this task to an existing DAG. A DAG is a series of tasks that starts with a scheduled root task and is linked together by dependencies.",
		ConflictsWith: []string{"schedule"},
	},
	"when": {
		Type:        schema.TypeString,
		Optional:    true,
		Description: "Specifies a Boolean SQL expression; multiple conditions joined with AND/OR are supported.",
	},
	"sql_statement": {
		Type:             schema.TypeString,
		Required:         true,
		Description:      "Any single SQL statement, or a call to a stored procedure, executed when the task runs.",
		ForceNew:         false,
		DiffSuppressFunc: DiffSuppressStatement,
	},
	"user_task_managed_initial_warehouse_size": {
		Type:     schema.TypeString,
		Optional: true,
		ValidateFunc: validation.StringInSlice([]string{
			"XSMALL", "X-SMALL", "SMALL", "MEDIUM", "LARGE", "XLARGE", "X-LARGE", "XXLARGE", "X2LARGE", "2X-LARGE",
		}, true),
		Description:   "Specifies the size of the compute resources to provision for the first run of the task, before a task history is available for Snowflake to determine an ideal size. Once a task has successfully completed a few runs, Snowflake ignores this parameter setting. (Conflicts with warehouse)",
		ConflictsWith: []string{"warehouse"},
	},
	"error_integration": {
		Type:        schema.TypeString,
		Optional:    true,
		Description: "Specifies the name of the notification integration used for error notifications.",
	},
	"allow_overlapping_execution": {
		Type:        schema.TypeBool,
		Optional:    true,
		Default:     false,
		Description: "By default, Snowflake ensures that only one instance of a particular DAG is allowed to run at a time, setting the parameter value to TRUE permits DAG runs to overlap.",
	},
}

type taskID struct {
	DatabaseName string
	SchemaName   string
	TaskName     string
}

// String() takes in a taskID object and returns a pipe-delimited string:
// DatabaseName|SchemaName|TaskName.
func (t *taskID) String() (string, error) {
	var buf bytes.Buffer
	csvWriter := csv.NewWriter(&buf)
	csvWriter.Comma = taskIDDelimiter
	dataIdentifiers := [][]string{{t.DatabaseName, t.SchemaName, t.TaskName}}
	err := csvWriter.WriteAll(dataIdentifiers)
	if err != nil {
		return "", err
	}
	strTaskID := strings.TrimSpace(buf.String())
	return strTaskID, nil
}

// difference find keys in 'a' but not in 'b'.
func difference(a, b map[string]interface{}) map[string]interface{} {
	diff := make(map[string]interface{})
	for k := range a {
		if _, ok := b[k]; !ok {
			diff[k] = a[k]
		}
	}
	return diff
}

// taskIDFromString() takes in a pipe-delimited string: DatabaseName|SchemaName|TaskName
// and returns a taskID object.
func taskIDFromString(stringID string) (*taskID, error) {
	reader := csv.NewReader(strings.NewReader(stringID))
	reader.Comma = pipeIDDelimiter
	lines, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("not CSV compatible")
	}

	if len(lines) != 1 {
		return nil, fmt.Errorf("1 line per task")
	}
	if len(lines[0]) != 3 {
		return nil, fmt.Errorf("3 fields allowed")
	}

	taskResult := &taskID{
		DatabaseName: lines[0][0],
		SchemaName:   lines[0][1],
		TaskName:     lines[0][2],
	}
	return taskResult, nil
}

// Task returns a pointer to the resource representing a task.
func Task() *schema.Resource {
	return &schema.Resource{
		Create: CreateTask,
		Read:   ReadTask,
		Update: UpdateTask,
		Delete: DeleteTask,

		Schema: taskSchema,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
	}
}

// ReadTask implements schema.ReadFunc.
func ReadTask(d *schema.ResourceData, meta interface{}) error {
	db := meta.(*sql.DB)
	taskID, err := taskIDFromString(d.Id())
	if err != nil {
		return err
	}

	database := taskID.DatabaseName
	schema := taskID.SchemaName
	name := taskID.TaskName

	builder := snowflake.Task(name, database, schema)
	q := builder.Show()
	row := snowflake.QueryRow(db, q)
	t, err := snowflake.ScanTask(row)
	if err == sql.ErrNoRows {
		// If not found, mark resource to be removed from state file during apply or refresh
		log.Printf("[DEBUG] task (%s) not found", d.Id())
		d.SetId("")
		return nil
	}
	if err != nil {
		return err
	}

	err = d.Set("enabled", t.IsEnabled())
	if err != nil {
		return err
	}

	err = d.Set("name", t.Name)
	if err != nil {
		return err
	}

	err = d.Set("database", t.DatabaseName)
	if err != nil {
		return err
	}

	err = d.Set("schema", t.SchemaName)
	if err != nil {
		return err
	}

	err = d.Set("warehouse", t.Warehouse)
	if err != nil {
		return err
	}

	err = d.Set("schedule", t.Schedule)
	if err != nil {
		return err
	}

	err = d.Set("comment", t.Comment)
	if err != nil {
		return err
	}

	allowOverlappingExecutionValue, err := t.AllowOverlappingExecution.Value()
	if err != nil {
		return err
	}

	if allowOverlappingExecutionValue != nil && allowOverlappingExecutionValue.(string) != "null" {
		allowOverlappingExecution, err := strconv.ParseBool(allowOverlappingExecutionValue.(string))
		if err != nil {
			return err
		}
		err = d.Set("allow_overlapping_execution", allowOverlappingExecution)
		if err != nil {
			return err
		}
	} else {
		err = d.Set("allow_overlapping_execution", false)
		if err != nil {
			return err
		}
	}

	// The "DESCRIBE TASK ..." command returns the string "null" for error_integration
	if t.ErrorIntegration.String == "null" {
		t.ErrorIntegration.Valid = false
		t.ErrorIntegration.String = ""
	}
	err = d.Set("error_integration", t.ErrorIntegration.String)
	if err != nil {
		return err
	}

	predecessors, err := t.GetPredecessors()
	if err != nil {
		return err
	}
	err = d.Set("after", predecessors)
	if err != nil {
		return err
	}

	err = d.Set("when", t.Condition)
	if err != nil {
		return err
	}

	err = d.Set("sql_statement", t.Definition)
	if err != nil {
		return err
	}

	q = builder.ShowParameters()
	paramRows, err := snowflake.Query(db, q)
	if err != nil {
		return err
	}
	params, err := snowflake.ScanTaskParameters(paramRows)
	if err != nil {
		return err
	}

	if len(params) > 0 {
		sessionParameters := map[string]interface{}{}
		fieldParameters := map[string]interface{}{
			"user_task_managed_initial_warehouse_size": "",
		}

		for _, param := range params {
			log.Printf("[TRACE] %+v\n", param)

			if param.Level != "TASK" {
				continue
			}
			switch param.Key {
			case "USER_TASK_MANAGED_INITIAL_WAREHOUSE_SIZE":
				fieldParameters["user_task_managed_initial_warehouse_size"] = param.Value
			case "USER_TASK_TIMEOUT_MS":
				timeout, err := strconv.ParseInt(param.Value, 10, 64)
				if err != nil {
					return err
				}

				fieldParameters["user_task_timeout_ms"] = timeout
			default:
				sessionParameters[param.Key] = param.Value
			}
		}

		err := d.Set("session_parameters", sessionParameters)
		if err != nil {
			return err
		}

		for key, value := range fieldParameters {
			//lintignore:R001
			err = d.Set(key, value)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// CreateTask implements schema.CreateFunc.
func CreateTask(d *schema.ResourceData, meta interface{}) error {
	var err error
	db := meta.(*sql.DB)
	database := d.Get("database").(string)
	schema := d.Get("schema").(string)
	name := d.Get("name").(string)
	sql := d.Get("sql_statement").(string)
	enabled := d.Get("enabled").(bool)

	builder := snowflake.Task(name, database, schema)
	builder.WithStatement(sql)

	// Set optionals
	if v, ok := d.GetOk("warehouse"); ok {
		builder.WithWarehouse(v.(string))
	}

	if v, ok := d.GetOk("user_task_managed_initial_warehouse_size"); ok {
		builder.WithInitialWarehouseSize(v.(string))
	}

	if v, ok := d.GetOk("schedule"); ok {
		builder.WithSchedule(v.(string))
	}

	if v, ok := d.GetOk("session_parameters"); ok {
		builder.WithSessionParameters(v.(map[string]interface{}))
	}

	if v, ok := d.GetOk("user_task_timeout_ms"); ok {
		builder.WithTimeout(v.(int))
	}

	if v, ok := d.GetOk("comment"); ok {
		builder.WithComment(v.(string))
	}

	if v, ok := d.GetOk("allow_overlapping_execution"); ok {
		builder.WithAllowOverlappingExecution(v.(bool))
	}

	if v, ok := d.GetOk("error_integration"); ok {
		builder.WithErrorIntegration(v.(string))
	}

	if v, ok := d.GetOk("after"); ok {
		after := expandStringList(v.([]interface{}))
		for _, dep := range after {
			rootTasks, err := snowflake.GetRootTasks(dep, database, schema, db)
			if err != nil {
				return err
			}
			for _, rootTask := range rootTasks {
				// if a root task is enabled, then it needs to be suspended before the child tasks can be created
				if rootTask.IsEnabled() {
					q := rootTask.Suspend()
					err = snowflake.Exec(db, q)
					if err != nil {
						return err
					}

					// resume the task after modifications are complete as long as it is not a standalone task
					if !(rootTask.Name == name) {
						defer func() {
							q = rootTask.Resume()
							err = snowflake.Exec(db, q)
							if err != nil {
								log.Printf("[WARN] failed to resume task %s", rootTask.Name)
							}
						}()
					}
				}
			}

			builder.WithAfter(after)
		}
	}

	if v, ok := d.GetOk("when"); ok {
		builder.WithCondition(v.(string))
	}

	q := builder.Create()
	err = snowflake.Exec(db, q)
	if err != nil {
		return errors.Wrapf(err, "error creating task %v", name)
	}

	taskID := &taskID{
		DatabaseName: database,
		SchemaName:   schema,
		TaskName:     name,
	}
	dataIDInput, err := taskID.String()
	if err != nil {
		return err
	}
	d.SetId(dataIDInput)

	if enabled {
		err := snowflake.WaitResumeTask(db, name, database, schema)
		if err != nil {
			log.Printf("[WARN] failed to resume task %s", name)
		}
	}

	return ReadTask(d, meta)
}

// UpdateTask implements schema.UpdateFunc.
func UpdateTask(d *schema.ResourceData, meta interface{}) error {
	taskID, err := taskIDFromString(d.Id())
	if err != nil {
		return err
	}

	db := meta.(*sql.DB)
	database := taskID.DatabaseName
	schema := taskID.SchemaName
	name := taskID.TaskName
	builder := snowflake.Task(name, database, schema)

	rootTasks, err := snowflake.GetRootTasks(name, database, schema, db)
	if err != nil {
		return err
	}
	for _, rootTask := range rootTasks {
		// if a root task is enabled, then it needs to be suspended before the child tasks can be created
		if rootTask.IsEnabled() {
			q := rootTask.Suspend()
			err = snowflake.Exec(db, q)
			if err != nil {
				return err
			}

			if !(rootTask.Name == name) {
				// resume the task after modifications are complete, as long as it is not a standalone task
				defer func() {
					q = rootTask.Resume()
					err = snowflake.Exec(db, q)
					if err != nil {
						log.Printf("[WARN] failed to resume task %s", rootTask.Name)
					}
				}()
			}
		}
	}

	if d.HasChange("warehouse") {
		var q string
		newWarehouse := d.Get("warehouse")

		if newWarehouse == "" {
			q = builder.SwitchWarehouseToManaged()
		} else {
			q = builder.ChangeWarehouse(newWarehouse.(string))
		}

		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating warehouse on task %v", d.Id())
		}
	}

	if d.HasChange("user_task_managed_initial_warehouse_size") {
		newSize := d.Get("user_task_managed_initial_warehouse_size")
		warehouse := d.Get("warehouse")

		if warehouse == "" && newSize != "" {
			var q = builder.SwitchManagedWithInitialSize(newSize.(string))
			err := snowflake.Exec(db, q)
			if err != nil {
				return errors.Wrapf(err, "error updating user_task_managed_initial_warehouse_size on task %v", d.Id())
			}
		}
	}

	if d.HasChange("error_integration") {
		var q string
		if errorIntegration, ok := d.GetOk("error_integration"); ok {
			q = builder.ChangeErrorIntegration(errorIntegration.(string))
		} else {
			q = builder.RemoveErrorIntegration()
		}
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating task error_integration on %v", d.Id())
		}
	}

	if d.HasChange("after") {
		// preemitvely removing schedule because a task cannot have both after and schedule
		q := builder.RemoveSchedule()
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating schedule on task %v", d.Id())
		}

		// making changes to after require suspending the current task
		q = builder.Suspend()
		err = snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error suspending task %v", d.Id())
		}

		old, new := d.GetChange("after")
		var oldAfter []string
		if len(old.([]interface{})) > 0 {
			oldAfter = expandStringList(old.([]interface{}))
		}

		var newAfter []string
		if len(new.([]interface{})) > 0 {
			newAfter = expandStringList(new.([]interface{}))
		}

		// Remove old dependencies that are not in new dependencies
		var toRemove []string
		for _, dep := range oldAfter {
			if !slices.Contains(newAfter, dep) {
				toRemove = append(toRemove, dep)
			}
		}
		if len(toRemove) > 0 {
			q := builder.RemoveAfter(toRemove)
			err := snowflake.Exec(db, q)
			if err != nil {
				return errors.Wrapf(err, "error removing after dependencies from task %v", d.Id())
			}
		}

		// Add new dependencies that are not in old dependencies
		var toAdd []string
		for _, dep := range newAfter {
			if !slices.Contains(oldAfter, dep) {
				toAdd = append(toAdd, dep)
			}
		}
		if len(toAdd) > 0 {
			// need to suspend any new root tasks from dependencies before adding them
			for _, dep := range toAdd {
				rootTasks, err := snowflake.GetRootTasks(dep, database, schema, db)
				if err != nil {
					return err
				}
				for _, rootTask := range rootTasks {
					if rootTask.IsEnabled() {
						q := rootTask.Suspend()
						err = snowflake.Exec(db, q)
						if err != nil {
							return err
						}

						if !(rootTask.Name == name) {
							// resume the task after modifications are complete, as long as it is not a standalone task
							defer func() {
								q = rootTask.Resume()
								err = snowflake.Exec(db, q)
								if err != nil {
									log.Printf("[WARN] failed to resume task %s", rootTask.Name)
								}
							}()
						}
					}
				}
			}
			q := builder.AddAfter(toAdd)
			err := snowflake.Exec(db, q)
			if err != nil {
				return errors.Wrapf(err, "error adding after dependencies to task %v", d.Id())
			}
		}
	}

	if d.HasChange("schedule") {
		var q string
		old, new := d.GetChange("schedule")
		if old != "" && new == "" {
			q = builder.RemoveSchedule()
		} else {
			q = builder.ChangeSchedule(new.(string))
		}
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating schedule on task %v", d.Id())
		}
	}

	if d.HasChange("user_task_timeout_ms") {
		var q string
		old, new := d.GetChange("user_task_timeout_ms")
		if old.(int) > 0 && new.(int) == 0 {
			q = builder.RemoveTimeout()
		} else {
			q = builder.ChangeTimeout(new.(int))
		}
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating user task timeout on task %v", d.Id())
		}
	}

	if d.HasChange("comment") {
		var q string
		old, new := d.GetChange("comment")
		if old != "" && new == "" {
			q = builder.RemoveComment()
		} else {
			q = builder.ChangeComment(new.(string))
		}
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating comment on task %v", d.Id())
		}
	}

	if d.HasChange("allow_overlapping_execution") {
		var q string
		_, new := d.GetChange("allow_overlapping_execution")
		flag := new.(bool)
		if flag {
			q = builder.SetAllowOverlappingExecutionParameter()
		} else {
			q = builder.UnsetAllowOverlappingExecutionParameter()
		}
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating task %v", d.Id())
		}
	}

	if d.HasChange("session_parameters") {
		var q string
		o, n := d.GetChange("session_parameters")

		if o == nil {
			o = make(map[string]interface{})
		}
		if n == nil {
			n = make(map[string]interface{})
		}
		os := o.(map[string]interface{})
		ns := n.(map[string]interface{})

		remove := difference(os, ns)
		add := difference(ns, os)

		if len(remove) > 0 {
			q = builder.RemoveSessionParameters(remove)
			err := snowflake.Exec(db, q)
			if err != nil {
				return errors.Wrapf(err, "error removing session_parameters on task %v", d.Id())
			}
		}

		if len(add) > 0 {
			q = builder.AddSessionParameters(add)
			err := snowflake.Exec(db, q)
			if err != nil {
				return errors.Wrapf(err, "error adding session_parameters to task %v", d.Id())
			}
		}
	}

	if d.HasChange("when") {
		new := d.Get("when")
		q := builder.ChangeCondition(new.(string))
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating when condition on task %v", d.Id())
		}
	}

	if d.HasChange("sql_statement") {
		new := d.Get("sql_statement")
		q := builder.ChangeSQLStatement(new.(string))
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating sql statement on task %v", d.Id())
		}
	}

	enabled := d.Get("enabled").(bool)
	if enabled {
		err := snowflake.WaitResumeTask(db, name, database, schema)
		if err != nil {
			log.Printf("[WARN] failed to resume task %s", name)
		}
	} else {
		q := builder.Suspend()
		err := snowflake.Exec(db, q)
		if err != nil {
			return errors.Wrapf(err, "error updating task state %v", d.Id())
		}
	}
	return ReadTask(d, meta)
}

// DeleteTask implements schema.DeleteFunc.
func DeleteTask(d *schema.ResourceData, meta interface{}) error {
	db := meta.(*sql.DB)
	taskID, err := taskIDFromString(d.Id())
	if err != nil {
		return err
	}

	database := taskID.DatabaseName
	schema := taskID.SchemaName
	name := taskID.TaskName

	rootTasks, err := snowflake.GetRootTasks(name, database, schema, db)
	if err != nil {
		return err
	}
	for _, rootTask := range rootTasks {
		// if a root task is enabled, then it needs to be suspended before the child tasks can be deleted
		if rootTask.IsEnabled() {
			q := rootTask.Suspend()
			err = snowflake.Exec(db, q)
			if err != nil {
				return err
			}

			if !(rootTask.Name == name) {
				// resume the task after modifications are complete, as long as it is not a standalone task
				defer func() {
					q = rootTask.Resume()
					err = snowflake.Exec(db, q)
					if err != nil {
						log.Printf("[WARN] failed to resume task %s", rootTask.Name)
					}
				}()
			}
		}
	}

	q := snowflake.Task(name, database, schema).Drop()
	err = snowflake.Exec(db, q)
	if err != nil {
		return errors.Wrapf(err, "error deleting task %v", d.Id())
	}

	d.SetId("")

	return nil
}
