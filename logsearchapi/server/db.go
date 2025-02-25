// Copyright (C) 2022, MinIO, Inc.
//
// This code is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License, version 3,
// as published by the Free Software Foundation.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License, version 3,
// along with this program.  If not, see <http://www.gnu.org/licenses/>

package server

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/georgysavva/scany/sqlscan"
)

// QTemplate is used to represent queries that involve string substitution as
// well as SQL positional argument substitution.
type QTemplate string

func (t QTemplate) build(args ...interface{}) string {
	return fmt.Sprintf(string(t), args...)
}

// Table a database table
type Table struct {
	Name            string
	CreateStatement QTemplate
}

func (t *Table) getCreateStatement() string {
	return t.CreateStatement.build(t.Name)
}

var (
	auditLogEventsTable = Table{
		Name: "audit_log_events",
		CreateStatement: `CREATE TABLE IF NOT EXISTS %s (
                                    event_time TIMESTAMPTZ NOT NULL,
                                    log JSONB NOT NULL
                                  ) PARTITION BY RANGE (event_time);`,
	}
	requestInfoTable = Table{
		Name: "request_info",
		CreateStatement: `CREATE TABLE IF NOT EXISTS %s (
                                    time TIMESTAMPTZ NOT NULL,
                                    api_name TEXT NOT NULL,
                                    access_key TEXT,
                                    bucket TEXT,
                                    object TEXT,
                                    time_to_response_ns INT8,
                                    remote_host TEXT,
                                    request_id TEXT,
                                    user_agent TEXT,
                                    response_status TEXT,
                                    response_status_code INT8,
                                    request_content_length INT8,
                                    response_content_length INT8
                                  ) PARTITION BY RANGE (time);`,
	}

	// Allows iterating on all tables
	allTables = []Table{auditLogEventsTable, requestInfoTable}
)

// DBClient is a client object that makes requests to the DB.
type DBClient struct {
	*sql.DB
}

// NewDBClient creates a new DBClient.
func NewDBClient(ctx context.Context, connStr string) (*DBClient, error) {
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	log.Print("Connected to db.")

	return &DBClient{db}, nil
}

func (c *DBClient) checkTableExists(ctx context.Context, table string) (bool, error) {
	const existsQuery QTemplate = `SELECT 1 FROM %s WHERE false;`
	_, err := c.QueryContext(ctx, existsQuery.build(table))
	if err != nil {
		// check for table does not exist error
		if strings.Contains(err.Error(), fmt.Sprintf(`relation "%s" does not exist`, table)) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *DBClient) checkPartitionTableExists(ctx context.Context, table string, givenTime time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	p := newPartitionTimeRange(givenTime)
	partitionTable := fmt.Sprintf("%s_%s", table, p.getPartnameSuffix())
	return c.checkTableExists(ctx, partitionTable)
}

func (c *DBClient) createTablePartition(ctx context.Context, table Table, givenTime time.Time) error {
	partTimeRange := newPartitionTimeRange(givenTime)
	_, err := c.ExecContext(ctx, table.getCreatePartitionStatement(partTimeRange))
	return err
}

func (c *DBClient) createTableAndPartition(ctx context.Context, table Table) error {
	if _, err := c.ExecContext(ctx, table.getCreateStatement()); err != nil {
		return err
	}

	// Tables are partitioned such that there are 4 partitions per month. At
	// startup we create the partitions for the current time along with the
	// "previous" and next "partitions". The partition for the past is created
	// only to enable some amount of manual data insertion via a script.
	now := time.Now()
	partitionNow := newPartitionTimeRange(now)
	partitionTimes := []time.Time{
		partitionNow.previous().StartDate,
		now,
		partitionNow.next().StartDate,
	}
	for _, pt := range partitionTimes {
		if err := c.createTablePartition(ctx, table, pt); err != nil {
			return err
		}
	}
	return nil
}

func (c *DBClient) createTables(ctx context.Context) error {
	for _, table := range allTables {
		if err := c.createTableAndPartition(ctx, table); err != nil {
			return err
		}
	}
	return nil
}

// InitDBTables Creates tables in the DB.
func (c *DBClient) InitDBTables(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	return c.createTables(ctx)
}

// InsertEvent inserts audit event in the DB.
func (c *DBClient) InsertEvent(ctx context.Context, eventBytes []byte) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if isEmptyEvent(eventBytes) {
		return nil
	}

	// Log the event-data if we are unable to save it in db for some reason.
	defer func() {
		if err != nil {
			log.Printf("audit event not saved: %s (cause: %v)", string(eventBytes), err)
		}
	}()

	event, err := parseJSONEvent(eventBytes)
	if err != nil {
		return err
	}

	const (
		insertAuditLogEvent QTemplate = `INSERT INTO %s (event_time, log) VALUES ($1, $2);`
		insertRequestInfo   QTemplate = `INSERT INTO %s (time,
                                                                 api_name,
                                                                 access_key,
                                                                 bucket,
                                                                 object,
                                                                 time_to_response_ns,
                                                                 remote_host,
                                                                 request_id,
                                                                 user_agent,
                                                                 response_status,
                                                                 response_status_code,
                                                                 request_content_length,
                                                                 response_content_length)
                                                   VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);`
	)

	// Start a database transaction
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// NOTE: Timestamps are nanosecond resolution from MinIO, however we are
	// using storing it with only microsecond precision in PG for simplicity
	// as that is the maximum precision supported by it.
	eventJSON, errJSON := json.Marshal(event)
	if errJSON != nil {
		return errJSON
	}
	_, err = tx.ExecContext(ctx, insertAuditLogEvent.build(auditLogEventsTable.Name), event.Time, eventJSON)
	if err != nil {
		return err
	}

	var reqLen *uint64
	rqlen, err := event.getRequestContentLength()
	if err == nil {
		reqLen = &rqlen
	}
	var respLen *uint64
	rsplen, err := event.getResponseContentLength()
	if err == nil {
		respLen = &rsplen
	}

	_, err = tx.ExecContext(ctx, insertRequestInfo.build(requestInfoTable.Name),
		event.Time,
		event.API.Name,
		event.API.AccessKey,
		event.API.Bucket,
		event.API.Object,
		event.API.TimeToResponse,
		event.RemoteHost,
		event.RequestID,
		event.UserAgent,
		event.API.Status,
		event.API.StatusCode,
		reqLen,
		respLen)
	if err != nil {
		return err
	}

	return tx.Commit()
}

type logEventRawRow struct {
	EventTime time.Time
	Log       string
}

// LogEventRow holds a raw log record
type LogEventRow struct {
	EventTime time.Time              `json:"event_time"`
	Log       map[string]interface{} `json:"log"`
}

// ReqInfoRow holds a structured log record
type ReqInfoRow struct {
	Time                  time.Time `json:"time"`
	APIName               string    `json:"api_name"`
	AccessKey             string    `json:"access_key"`
	Bucket                string    `json:"bucket"`
	Object                string    `json:"object"`
	TimeToResponseNs      uint64    `json:"time_to_response_ns"`
	RemoteHost            string    `json:"remote_host"`
	RequestID             string    `json:"request_id"`
	UserAgent             string    `json:"user_agent"`
	ResponseStatus        string    `json:"response_status"`
	ResponseStatusCode    int       `json:"response_status_code"`
	RequestContentLength  *uint64   `json:"request_content_length"`
	ResponseContentLength *uint64   `json:"response_content_length"`
}

func iPtrToStr(i *uint64) string {
	if i == nil {
		return ""
	}
	return fmt.Sprintf("%d", *i)
}

// Search executes a search query on the db.
func (c *DBClient) Search(ctx context.Context, s *SearchQuery, w io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var (
		logEventCSVHeader = []string{"event_time", "log"}
		reqInfoCSVHeader  = []string{
			"time",
			"api_name",
			"access_key",
			"bucket",
			"object",
			"time_to_response_ns",
			"remote_host",
			"request_id",
			"user_agent",
			"response_status",
			"response_status_code",
			"request_content_length",
			"response_content_length",
		}
	)

	const (
		logEventSelect QTemplate = `SELECT event_time,
                                                   log
                                              FROM %s
                                             %s
                                          ORDER BY event_time %s
                                            %s;`

		reqInfoSelect QTemplate = `SELECT time,
                                                  api_name,
                                                  access_key,
                                                  bucket,
                                                  object,
                                                  time_to_response_ns,
                                                  remote_host,
                                                  request_id,
                                                  user_agent,
                                                  response_status,
                                                  response_status_code,
                                                  request_content_length,
                                                  response_content_length
                                             FROM %s
                                            %s
                                         	ORDER BY time %s
                                           	%s;`
	)

	timeOrder := "DESC"
	if s.TimeAscending {
		timeOrder = "ASC"
	}

	switch s.Query {
	case rawQ:
		sqlArgs := []interface{}{}
		dollarStart := 1
		whereClauses := []string{}
		// only filter by time if provided
		if s.TimeStart != nil {
			timeRangeClause := fmt.Sprintf("event_time >= $%d", dollarStart)
			sqlArgs = append(sqlArgs, s.TimeStart.Format(time.RFC3339Nano))
			whereClauses = append(whereClauses, timeRangeClause)
			dollarStart++
		}
		if s.TimeEnd != nil {
			timeRangeClause := fmt.Sprintf("event_time < $%d", dollarStart)
			sqlArgs = append(sqlArgs, s.TimeEnd.Format(time.RFC3339Nano))
			whereClauses = append(whereClauses, timeRangeClause)
			dollarStart++
		}
		if s.LastDuration != nil {
			// s.TimeEnd and s.TimeStart would be nil due to
			// validation of s.
			durationSeconds := int64(s.LastDuration.Seconds())
			timeRangeClause := fmt.Sprintf("event_time >= CURRENT_TIMESTAMP - '%d seconds'::interval", durationSeconds)
			whereClauses = append(whereClauses, timeRangeClause)
		}

		// Remaining dollar params are added for filter where clauses
		filterClauses, filterArgs, dollarStart := generateFilterClauses(s.FParams, dollarStart)
		whereClauses = append(whereClauses, filterClauses...)
		sqlArgs = append(sqlArgs, filterArgs...)

		whereClause := strings.Join(whereClauses, " AND ")
		if len(whereClauses) > 0 {
			whereClause = fmt.Sprintf("WHERE %s", whereClause)
		}

		pagingClause := ""
		if s.ExportFormat == "" {
			sqlArgs = append(sqlArgs, s.PageNumber*s.PageSize, s.PageSize)
			pagingClause = fmt.Sprintf("OFFSET $%d LIMIT $%d", dollarStart, dollarStart+1)
		}

		q := logEventSelect.build(auditLogEventsTable.Name, whereClause, timeOrder, pagingClause)
		rows, err := c.QueryContext(ctx, q, sqlArgs...)
		if err != nil {
			return fmt.Errorf("Error querying db: %v", err)
		}
		defer rows.Close()

		switch s.ExportFormat {
		case "ndjson":
			jw := json.NewEncoder(w)
			for rows.Next() {
				var logEventRaw logEventRawRow
				if err := sqlscan.ScanRow(&logEventRaw, rows); err != nil {
					return fmt.Errorf("Error accessing db: %v", err)
				}
				var logEvent LogEventRow
				logEvent.EventTime = logEventRaw.EventTime
				logEvent.Log = make(map[string]interface{})
				if err := json.Unmarshal([]byte(logEventRaw.Log), &logEvent.Log); err != nil {
					return fmt.Errorf("Error decoding json log: %v", err)
				}
				if err := jw.Encode(logEvent); err != nil {
					return fmt.Errorf("Error writing to output stream: %v", err)
				}
			}

		case "csv":
			cw := csv.NewWriter(w)

			// Write CSV header
			if err := cw.Write(logEventCSVHeader); err != nil {
				return fmt.Errorf("Error writing to output stream: %v", err)
			}

			// Write rows
			for rows.Next() {
				var logEventRaw logEventRawRow
				if err := sqlscan.ScanRow(&logEventRaw, rows); err != nil {
					return fmt.Errorf("Error accessing db: %v", err)
				}
				record := []string{
					logEventRaw.EventTime.Format(time.RFC3339Nano),
					logEventRaw.Log,
				}
				if err := cw.Write(record); err != nil {
					return fmt.Errorf("Error writing to output stream: %v", err)
				}
			}
			cw.Flush()
			if err := cw.Error(); err != nil {
				return fmt.Errorf("Error writing to output stream: %v", err)
			}

		default:
			// Send out one page of results in response.
			var logEventsRaw []logEventRawRow
			if err := sqlscan.ScanAll(&logEventsRaw, rows); err != nil {
				return fmt.Errorf("Error accessing db: %v", err)
			}
			// parse the encoded json string stored in the db into a json
			// object for output
			logEvents := make([]LogEventRow, len(logEventsRaw))
			for i, e := range logEventsRaw {
				logEvents[i].EventTime = e.EventTime
				logEvents[i].Log = make(map[string]interface{})
				if err := json.Unmarshal([]byte(e.Log), &logEvents[i].Log); err != nil {
					return fmt.Errorf("Error decoding json log: %v", err)
				}
			}
			jw := json.NewEncoder(w)
			if err := jw.Encode(logEvents); err != nil {
				return fmt.Errorf("Error writing to output stream: %v", err)
			}
		}

	case reqInfoQ:
		sqlArgs := []interface{}{}
		dollarStart := 1
		whereClauses := []string{}
		// only filter by time if provided
		if s.TimeStart != nil {
			timeRangeClause := fmt.Sprintf("time >= $%d", dollarStart)
			sqlArgs = append(sqlArgs, s.TimeStart.Format(time.RFC3339Nano))
			whereClauses = append(whereClauses, timeRangeClause)
			dollarStart++
		}
		// only filter by time if provided
		if s.TimeEnd != nil {
			timeRangeClause := fmt.Sprintf("time < $%d", dollarStart)
			sqlArgs = append(sqlArgs, s.TimeEnd.Format(time.RFC3339Nano))
			whereClauses = append(whereClauses, timeRangeClause)
			dollarStart++
		}
		if s.LastDuration != nil {
			// s.TimeEnd and s.TimeStart would be nil due to
			// validation of s.
			durationSeconds := int64(s.LastDuration.Seconds())
			timeRangeClause := fmt.Sprintf("event_time >= CURRENT_TIMESTAMP - '%d seconds'::interval", durationSeconds)
			whereClauses = append(whereClauses, timeRangeClause)
		}

		// Remaining dollar params are added for filter where clauses
		filterClauses, filterArgs, dollarStart := generateFilterClauses(s.FParams, dollarStart)
		whereClauses = append(whereClauses, filterClauses...)
		sqlArgs = append(sqlArgs, filterArgs...)

		whereClause := strings.Join(whereClauses, " AND ")
		if len(whereClauses) > 0 {
			whereClause = fmt.Sprintf("WHERE %s", whereClause)
		}

		pagingClause := ""
		if s.ExportFormat == "" {
			sqlArgs = append(sqlArgs, s.PageNumber*s.PageSize, s.PageSize)
			pagingClause = fmt.Sprintf("OFFSET $%d LIMIT $%d", dollarStart, dollarStart+1)
		}

		q := reqInfoSelect.build(requestInfoTable.Name, whereClause, timeOrder, pagingClause)
		rows, err := c.QueryContext(ctx, q, sqlArgs...)
		if err != nil {
			return fmt.Errorf("Error querying db: %v", err)
		}
		defer rows.Close()

		switch s.ExportFormat {
		case "ndjson":
			jw := json.NewEncoder(w)
			for rows.Next() {
				var reqInfo ReqInfoRow
				if err := sqlscan.ScanRow(&reqInfo, rows); err != nil {
					return fmt.Errorf("Error accessing db: %v", err)
				}
				if err := jw.Encode(reqInfo); err != nil {
					return fmt.Errorf("Error writing to output stream: %v", err)
				}
			}

		case "csv":
			cw := csv.NewWriter(w)

			// Write CSV header
			if err := cw.Write(reqInfoCSVHeader); err != nil {
				return fmt.Errorf("Error writing to output stream: %v", err)
			}

			// Write rows
			for rows.Next() {
				var i ReqInfoRow
				if err := sqlscan.ScanRow(&i, rows); err != nil {
					return fmt.Errorf("Error accessing db: %v", err)
				}
				record := []string{
					i.Time.Format(time.RFC3339Nano),
					i.APIName,
					i.AccessKey,
					i.Bucket,
					i.Object,
					fmt.Sprintf("%d", i.TimeToResponseNs),
					i.RemoteHost,
					i.RequestID,
					i.UserAgent,
					i.ResponseStatus,
					fmt.Sprintf("%d", i.ResponseStatusCode),
					iPtrToStr(i.RequestContentLength),
					iPtrToStr(i.ResponseContentLength),
				}
				if err := cw.Write(record); err != nil {
					return fmt.Errorf("Error writing to output stream: %v", err)
				}
			}
			cw.Flush()
			if err := cw.Error(); err != nil {
				return fmt.Errorf("Error writing to output stream: %v", err)
			}

		default:
			// Send out one page of results in response
			var reqInfos []ReqInfoRow
			if err := sqlscan.ScanAll(&reqInfos, rows); err != nil {
				return fmt.Errorf("Error accessing db: %v", err)
			}
			jw := json.NewEncoder(w)
			if err := jw.Encode(reqInfos); err != nil {
				return fmt.Errorf("Error writing to output stream: %v", err)
			}
		}
	default:
		return fmt.Errorf("Invalid query name: %v", s.Query)
	}
	return nil
}
