package pgengine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"time"

	"github.com/cybertec-postgresql/pg_timetable/internal/cmdparser"
	"github.com/jmoiron/sqlx"

	pgconn "github.com/jackc/pgconn"
	pgx "github.com/jackc/pgx/v4"
	stdlib "github.com/jackc/pgx/v4/stdlib"
)

// WaitTime specifies amount of time in seconds to wait before reconnecting to DB
const WaitTime = 5

// maximum wait time before reconnect attempts
const maxWaitTime = WaitTime * 16

// ConfigDb is the global database object
var ConfigDb *sqlx.DB

// ClientName is unique ifentifier of the scheduler application running
var ClientName string

// NoShellTasks parameter disables SHELL tasks executing
var NoShellTasks bool

var sqls = []string{sqlDDL, sqlJSONSchema, sqlTasks, sqlJobFunctions}
var sqlNames = []string{"DDL", "JSON Schema", "Built-in Tasks", "Job Functions"}

// Logger incapsulates Logger interface from pgx package
type Logger struct {
	pgx.Logger
}

// Log prints messages using native log levels
func (l Logger) Log(ctx context.Context, level pgx.LogLevel, msg string, data map[string]interface{}) {
	var s string
	switch level {
	case pgx.LogLevelTrace, pgx.LogLevelDebug, pgx.LogLevelInfo:
		s = "DEBUG"
	case pgx.LogLevelWarn:
		s = "NOTICE"
	case pgx.LogLevelError:
		s = "ERROR"
	default:
		s = "LOG"
	}
	j, _ := json.Marshal(data)
	s = fmt.Sprintf(GetLogPrefix(s), fmt.Sprint(msg, " ", string(j)))
	fmt.Println(s)
}

// OpenDB opens connection to the database
var OpenDB func(driverName string, dataSourceName string) (*sql.DB, error) = sql.Open

// flag indicates whether we finished bootstraping and now can call TryLockClientName()
var bootstraping bool = true

// TryLockClientName obtains lock on the server to prevent another client with the same name
func TryLockClientName(ctx context.Context, conn *pgconn.PgConn) error {
	if bootstraping {
		return nil
	}
	var wt int = WaitTime

	for {
		LogToDB(ctx, "DEBUG", fmt.Sprintf("Trying to get lock for '%s', client pid %d, server pid %d", ClientName, os.Getpid(), conn.PID()))
		sql := fmt.Sprintf("SELECT timetable.try_lock_client_name(%d, $worker$%s$worker$)", os.Getpid(), ClientName)
		LogToDB(ctx, "DEBUG", "Exec ", sql)
		multiresultsres := conn.Exec(ctx, sql)
		results, err := multiresultsres.ReadAll()
		if err != nil {
			LogToDB(ctx, "ERROR", "Error occurred during client name locking: ", err)
			return err
		} else if !bytes.Equal(results[0].Rows[0][0], []byte("t")) {
			LogToDB(ctx, "ERROR", "Another client is already connected to server with name: ", ClientName)
		} else {
			return nil
		}
		select {
		case <-time.After(time.Duration(wt) * time.Second):
			if wt < maxWaitTime {
				wt = wt * 2
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// InitAndTestConfigDBConnection opens connection and creates schema
func InitAndTestConfigDBConnection(ctx context.Context, cmdOpts cmdparser.CmdOptions) bool {
	ClientName = cmdOpts.ClientName
	NoShellTasks = cmdOpts.NoShellTasks
	VerboseLogLevel = cmdOpts.Verbose
	LogToDB(ctx, "DEBUG", fmt.Sprintf("Starting new session... %s", &cmdOpts))
	var wt int = WaitTime
	var err error

	connstr := fmt.Sprintf("application_name='pg_timetable' host='%s' port='%s' dbname='%s' sslmode='%s' user='%s' password='%s'",
		cmdOpts.Host, cmdOpts.Port, cmdOpts.Dbname, cmdOpts.SSLMode, cmdOpts.User, cmdOpts.Password)
	LogToDB(ctx, "DEBUG", "Connection string: ", connstr)
	connConfig, err := pgx.ParseConfig(connstr)
	if err != nil {
		LogToDB(ctx, "ERROR", err)
		return false
	}
	connConfig.OnNotice = func(c *pgconn.PgConn, n *pgconn.Notice) {
		LogToDB(ctx, "USER", "Severity: ", n.Severity, "; Message: ", n.Message)
	}
	if !cmdOpts.Debug {
		connConfig.AfterConnect = func(ctx context.Context, pgconn *pgconn.PgConn) error {
			if err := TryLockClientName(ctx, pgconn); err != nil {
				return err
			}
			return pgconn.Exec(ctx, "LISTEN "+ClientName).Close()
		}
		connConfig.OnNotification = NotificationHandler
	}
	connConfig.Logger = Logger{}
	if VerboseLogLevel {
		connConfig.LogLevel = pgx.LogLevelDebug
	} else {
		connConfig.LogLevel = pgx.LogLevelWarn
	}
	connConfig.PreferSimpleProtocol = true
	connstr = stdlib.RegisterConnConfig(connConfig)
	bootstraping = true
	db, err := OpenDB("pgx", connstr)
	if err == nil {
		err = db.PingContext(ctx)
	}
	for err != nil {
		LogToDB(ctx, "ERROR", err)
		LogToDB(ctx, "LOG", "Reconnecting in ", wt, " sec...")
		select {
		case <-time.After(time.Duration(wt) * time.Second):
			err = db.PingContext(ctx)
		case <-ctx.Done():
			LogToDB(ctx, "ERROR", "Connection request cancelled: ", ctx.Err())
			return false
		}
		if wt < maxWaitTime {
			wt = wt * 2
		}
	}

	LogToDB(ctx, "DEBUG", "Connection string: ", connstr)
	LogToDB(ctx, "LOG", "Connection established...")
	LogToDB(ctx, "LOG", fmt.Sprintf("Proceeding as '%s' with client PID %d", ClientName, os.Getpid()))

	ConfigDb = sqlx.NewDb(db, "pgx")
	if !ExecuteSchemaScripts(ctx) {
		return false
	}
	if cmdOpts.File != "" {
		if !ExecuteCustomScripts(ctx, cmdOpts.File) {
			return false
		}
	}

	return true
}

// ExecuteCustomScripts executes SQL scripts in files
func ExecuteCustomScripts(ctx context.Context, filename ...string) bool {
	for _, f := range filename {
		sql, err := ioutil.ReadFile(f)
		if err != nil {
			fmt.Printf(GetLogPrefixLn("PANIC"), err)
			return false
		}
		fmt.Printf(GetLogPrefixLn("LOG"), "Executing script: "+f)
		if _, err = ConfigDb.ExecContext(ctx, string(sql)); err != nil {
			fmt.Printf(GetLogPrefixLn("PANIC"), err)
			return false
		}
		LogToDB(ctx, "LOG", "Script file executed: "+f)
	}
	return true
}

// ExecuteSchemaScripts executes initial schema scripts
func ExecuteSchemaScripts(ctx context.Context) bool {
	var exists bool
	err := ConfigDb.GetContext(ctx, &exists, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname = 'timetable')")
	if err != nil {
		return false
	}
	bootstraping = false
	if !exists {
		conn, err := ConfigDb.DB.Conn(ctx)
		if err != nil {
			fmt.Printf(GetLogPrefixLn("PANIC"), err)
			return false
		}
		err = conn.Raw(func(driverConn interface{}) error {
			c := driverConn.(*stdlib.Conn).Conn()
			for i, sql := range sqls {
				sqlName := sqlNames[i]
				fmt.Printf(GetLogPrefixLn("LOG"), "Executing script: "+sqlName)
				if _, err = c.Exec(ctx, sql); err != nil {
					fmt.Printf(GetLogPrefixLn("PANIC"), err)
					fmt.Printf(GetLogPrefixLn("PANIC"), "Dropping \"timetable\" schema")
					_, err = c.Exec(ctx, "DROP SCHEMA IF EXISTS timetable CASCADE")
					if err != nil {
						fmt.Printf(GetLogPrefixLn("PANIC"), err)
					}
					return err
				}
				LogToDB(ctx, "LOG", "Schema file executed: "+sqlName)
			}
			LogToDB(ctx, "LOG", "Configuration schema created...")

			err = TryLockClientName(ctx, c.PgConn())
			return err
		})
		if err != nil {
			LogToDB(ctx, "ERROR", err)
			return false
		}
	}

	return true
}

// FinalizeConfigDBConnection closes session
func FinalizeConfigDBConnection() {
	fmt.Printf(GetLogPrefixLn("LOG"), "Closing session")
	_, err := ConfigDb.Exec("DELETE FROM timetable.active_session WHERE client_pid = $1 AND client_name = $2", os.Getpid(), ClientName)
	if err != nil {
		fmt.Printf(GetLogPrefixLn("ERROR"), "Cannot finalize database session: ", err)
	}
	if err := ConfigDb.Close(); err != nil {
		fmt.Printf(GetLogPrefixLn("ERROR"), "Error occurred during connection closing: ", err)
	}
	ConfigDb = nil
}

//ReconnectDbAndFixLeftovers keeps trying reconnecting every `waitTime` seconds till connection established
func ReconnectDbAndFixLeftovers(ctx context.Context) bool {
	for ConfigDb.PingContext(ctx) != nil {
		fmt.Printf(GetLogPrefixLn("REPAIR"),
			fmt.Sprintf("Connection to the server was lost. Waiting for %d sec...", WaitTime))
		select {
		case <-time.After(WaitTime * time.Second):
			fmt.Printf(GetLogPrefix("REPAIR"), "Reconnecting...\n")
		case <-ctx.Done():
			fmt.Printf(GetLogPrefixLn("ERROR"), fmt.Sprintf("request cancelled: %v", ctx.Err()))
			return false
		}
	}
	LogToDB(ctx, "LOG", "Connection reestablished...")
	FixSchedulerCrash(ctx)
	return true
}
