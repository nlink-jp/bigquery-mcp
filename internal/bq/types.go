// Package bq is a hand-written client for the BigQuery REST API v2
// (ADR-0001). It carries exactly the request and response fields this
// server reads or writes, decoded from the shapes published in the v2
// Discovery document; unknown fields are ignored.
package bq

import "encoding/json"

// TableRef identifies a table.
type TableRef struct {
	ProjectID string `json:"projectId"`
	DatasetID string `json:"datasetId"`
	TableID   string `json:"tableId"`
}

// String renders project.dataset.table.
func (r TableRef) String() string { return r.ProjectID + "." + r.DatasetID + "." + r.TableID }

// DatasetKey renders project.dataset.
func (r TableRef) DatasetKey() string { return r.ProjectID + "." + r.DatasetID }

// RoutineRef identifies a routine (UDF, procedure, table function).
type RoutineRef struct {
	ProjectID string `json:"projectId"`
	DatasetID string `json:"datasetId"`
	RoutineID string `json:"routineId"`
}

// String renders project.dataset.routine.
func (r RoutineRef) String() string { return r.ProjectID + "." + r.DatasetID + "." + r.RoutineID }

// DatasetKey renders project.dataset.
func (r RoutineRef) DatasetKey() string { return r.ProjectID + "." + r.DatasetID }

// FieldSchema is one column (TableFieldSchema), nested for RECORD.
type FieldSchema struct {
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Mode        string         `json:"mode,omitempty"`
	Description string         `json:"description,omitempty"`
	Fields      []*FieldSchema `json:"fields,omitempty"`
}

// TableSchema is the schema of a table or a result set.
type TableSchema struct {
	Fields []*FieldSchema `json:"fields"`
}

// ErrorProto is one BigQuery error entry.
type ErrorProto struct {
	Reason   string `json:"reason"`
	Location string `json:"location"`
	Message  string `json:"message"`
}

// apiErrorBody is the envelope of a non-2xx response.
type apiErrorBody struct {
	Error struct {
		Code    int          `json:"code"`
		Message string       `json:"message"`
		Status  string       `json:"status"`
		Errors  []ErrorProto `json:"errors"`
	} `json:"error"`
}

// QueryParameter is a named GoogleSQL query parameter (scalar types only in v1).
type QueryParameter struct {
	Name           string              `json:"name"`
	ParameterType  QueryParameterType  `json:"parameterType"`
	ParameterValue QueryParameterValue `json:"parameterValue"`
}

// QueryParameterType names a scalar type (STRING, INT64, FLOAT64, BOOL,
// NUMERIC, BIGNUMERIC, DATE, TIME, DATETIME, TIMESTAMP, BYTES, GEOGRAPHY, JSON).
type QueryParameterType struct {
	Type string `json:"type"`
}

// QueryParameterValue carries a scalar value as a string.
type QueryParameterValue struct {
	Value string `json:"value"`
}

// queryRequest is the jobs.query body.
type queryRequest struct {
	Query              string            `json:"query"`
	UseLegacySQL       bool              `json:"useLegacySql"`
	DryRun             bool              `json:"dryRun,omitempty"`
	Location           string            `json:"location,omitempty"`
	MaximumBytesBilled string            `json:"maximumBytesBilled,omitempty"`
	JobTimeoutMs       string            `json:"jobTimeoutMs,omitempty"`
	TimeoutMs          int64             `json:"timeoutMs,omitempty"`
	MaxResults         int64             `json:"maxResults,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
	UseQueryCache      *bool             `json:"useQueryCache,omitempty"`
	RequestID          string            `json:"requestId,omitempty"`
	ParameterMode      string            `json:"parameterMode,omitempty"`
	QueryParameters    []QueryParameter  `json:"queryParameters,omitempty"`
	FormatOptions      *formatOptions    `json:"formatOptions,omitempty"`
}

type formatOptions struct {
	UseInt64Timestamp bool `json:"useInt64Timestamp"`
}

// tableRow / tableCell are the wire shape of result rows: f[].v in schema order.
type tableRow struct {
	F []tableCell `json:"f"`
}

type tableCell struct {
	V json.RawMessage `json:"v"`
}

// jobReference is the identity of a job.
type jobReference struct {
	ProjectID string `json:"projectId"`
	JobID     string `json:"jobId"`
	Location  string `json:"location"`
}

// queryResponse is the jobs.query / jobs.getQueryResults response.
type queryResponse struct {
	JobReference        *jobReference `json:"jobReference"`
	JobComplete         bool          `json:"jobComplete"`
	Schema              *TableSchema  `json:"schema"`
	Rows                []tableRow    `json:"rows"`
	TotalRows           string        `json:"totalRows"`
	PageToken           string        `json:"pageToken"`
	TotalBytesProcessed string        `json:"totalBytesProcessed"`
	TotalBytesBilled    string        `json:"totalBytesBilled"`
	CacheHit            *bool         `json:"cacheHit"`
	TotalSlotMs         string        `json:"totalSlotMs"`
	StatementType       string        `json:"statementType"`
	Errors              []ErrorProto  `json:"errors"`
	QueryID             string        `json:"queryId"`
	Location            string        `json:"location"`
}

// insertJobRequest is the jobs.insert body used for the dry run that
// needs referencedTables.
type insertJobRequest struct {
	JobReference  *jobReference    `json:"jobReference,omitempty"`
	Configuration jobConfiguration `json:"configuration"`
}

type jobConfiguration struct {
	DryRun       bool                  `json:"dryRun"`
	JobTimeoutMs string                `json:"jobTimeoutMs,omitempty"`
	Labels       map[string]string     `json:"labels,omitempty"`
	Query        jobConfigurationQuery `json:"query"`
}

type jobConfigurationQuery struct {
	Query              string           `json:"query"`
	UseLegacySQL       bool             `json:"useLegacySql"`
	ParameterMode      string           `json:"parameterMode,omitempty"`
	QueryParameters    []QueryParameter `json:"queryParameters,omitempty"`
	MaximumBytesBilled string           `json:"maximumBytesBilled,omitempty"`
	UseQueryCache      *bool            `json:"useQueryCache,omitempty"`
	Priority           string           `json:"priority,omitempty"`
}

// jobResource is the subset of the Job resource read after jobs.insert.
type jobResource struct {
	JobReference *jobReference `json:"jobReference"`
	Status       struct {
		State       string       `json:"state"`
		ErrorResult *ErrorProto  `json:"errorResult"`
		Errors      []ErrorProto `json:"errors"`
	} `json:"status"`
	Statistics struct {
		CreationTime string `json:"creationTime"`
		StartTime    string `json:"startTime"`
		EndTime      string `json:"endTime"`
		TotalSlotMs  string `json:"totalSlotMs"`
		Query        struct {
			StatementType               string           `json:"statementType"`
			TotalBytesProcessed         string           `json:"totalBytesProcessed"`
			TotalBytesProcessedAccuracy string           `json:"totalBytesProcessedAccuracy"`
			TotalBytesBilled            string           `json:"totalBytesBilled"`
			TotalSlotMs                 string           `json:"totalSlotMs"`
			CacheHit                    *bool            `json:"cacheHit"`
			ReferencedTables            []TableRef       `json:"referencedTables"`
			ReferencedRoutines          []RoutineRef     `json:"referencedRoutines"`
			UndeclaredQueryParameters   []QueryParameter `json:"undeclaredQueryParameters"`
			Schema                      *TableSchema     `json:"schema"`
		} `json:"query"`
	} `json:"statistics"`
}

// TimePartitioning describes a time-partitioned table.
type TimePartitioning struct {
	Type                   string `json:"type"`
	Field                  string `json:"field,omitempty"`
	ExpirationMs           string `json:"expirationMs,omitempty"`
	RequirePartitionFilter bool   `json:"requirePartitionFilter,omitempty"`
}

// RangePartitioning describes an integer-range-partitioned table.
type RangePartitioning struct {
	Field string `json:"field"`
	Range struct {
		Start    string `json:"start"`
		End      string `json:"end"`
		Interval string `json:"interval"`
	} `json:"range"`
}

// Clustering lists the clustering columns.
type Clustering struct {
	Fields []string `json:"fields"`
}

// Dataset is one entry of datasets.list.
type Dataset struct {
	ProjectID string `json:"project_id"`
	DatasetID string `json:"dataset_id"`
	Location  string `json:"location,omitempty"`
}

// datasetListResponse is the wire shape of datasets.list.
type datasetListResponse struct {
	Datasets []struct {
		DatasetReference struct {
			ProjectID string `json:"projectId"`
			DatasetID string `json:"datasetId"`
		} `json:"datasetReference"`
		Location string `json:"location"`
	} `json:"datasets"`
	NextPageToken string `json:"nextPageToken"`
}

// TableSummary is one entry of tables.list.
type TableSummary struct {
	TableID         string `json:"table_id"`
	Type            string `json:"type"`
	PartitionColumn string `json:"partition_column,omitempty"`
	PartitionType   string `json:"partition_type,omitempty"`
}

// tableListResponse is the wire shape of tables.list.
type tableListResponse struct {
	Tables []struct {
		TableReference    TableRef           `json:"tableReference"`
		Type              string             `json:"type"`
		TimePartitioning  *TimePartitioning  `json:"timePartitioning"`
		RangePartitioning *RangePartitioning `json:"rangePartitioning"`
	} `json:"tables"`
	NextPageToken string `json:"nextPageToken"`
}

// TableInfo is the subset of the Table resource returned by tables.get.
type TableInfo struct {
	TableReference         TableRef           `json:"tableReference"`
	Type                   string             `json:"type"`
	Description            string             `json:"description,omitempty"`
	Schema                 *TableSchema       `json:"schema"`
	NumRows                string             `json:"numRows"`
	NumBytes               string             `json:"numBytes"`
	CreationTime           string             `json:"creationTime"`
	LastModifiedTime       string             `json:"lastModifiedTime"`
	ExpirationTime         string             `json:"expirationTime,omitempty"`
	TimePartitioning       *TimePartitioning  `json:"timePartitioning,omitempty"`
	RangePartitioning      *RangePartitioning `json:"rangePartitioning,omitempty"`
	Clustering             *Clustering        `json:"clustering,omitempty"`
	RequirePartitionFilter bool               `json:"requirePartitionFilter,omitempty"`
	Location               string             `json:"location,omitempty"`
	View                   *struct {
		Query string `json:"query"`
	} `json:"view,omitempty"`
}

// PartitionColumn returns the partition column and kind ("time" or
// "range") or empty strings when the table is not partitioned. An
// ingestion-time partitioned table (no field) reports the pseudo column
// _PARTITIONTIME.
func (t *TableInfo) PartitionColumn() (column, kind string) {
	return partitionColumn(t.TimePartitioning, t.RangePartitioning)
}

func partitionColumn(tp *TimePartitioning, rp *RangePartitioning) (column, kind string) {
	switch {
	case tp != nil && tp.Field != "":
		return tp.Field, "time"
	case tp != nil:
		return "_PARTITIONTIME", "time"
	case rp != nil:
		return rp.Field, "range"
	}
	return "", ""
}
