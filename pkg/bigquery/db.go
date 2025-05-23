package bigquery

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"cloud.google.com/go/bigquery"
	"github.com/bruin-data/bruin/pkg/pipeline"
	"github.com/bruin-data/bruin/pkg/query"
	"github.com/pkg/errors"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

var scopes = []string{
	bigquery.Scope,
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/drive",
}

type Querier interface {
	RunQueryWithoutResult(ctx context.Context, query *query.Query) error
	Ping(ctx context.Context) error
}
type Selector interface {
	Select(ctx context.Context, query *query.Query) ([][]interface{}, error)
	SelectWithSchema(ctx context.Context, queryObj *query.Query) (*query.QueryResult, error)
}

type MetadataUpdater interface {
	UpdateTableMetadataIfNotExist(ctx context.Context, asset *pipeline.Asset) error
}

type TableManager interface {
	IsPartitioningOrClusteringMismatch(ctx context.Context, meta *bigquery.TableMetadata, asset *pipeline.Asset) bool
	CreateDataSetIfNotExist(asset *pipeline.Asset, ctx context.Context) error
	IsMaterializationTypeMismatch(ctx context.Context, meta *bigquery.TableMetadata, asset *pipeline.Asset) bool
	DropTableOnMismatch(ctx context.Context, tableName string, asset *pipeline.Asset) error
	BuildTableExistsQuery(tableName string) (string, error)
}

type DB interface {
	Querier
	Selector
	MetadataUpdater
	TableManager
}

var (
	datasetNameCache sync.Map // Global cache for dataset existence
	datasetLocks     sync.Map // Global map for dataset-specific locks
)

type Client struct {
	client *bigquery.Client
	config *Config
}

func NewDB(c *Config) (*Client, error) {
	options := []option.ClientOption{
		option.WithScopes(scopes...),
	}

	switch {
	case c.CredentialsJSON != "":
		options = append(options, option.WithCredentialsJSON([]byte(c.CredentialsJSON)))
	case c.CredentialsFilePath != "":
		options = append(options, option.WithCredentialsFile(c.CredentialsFilePath))
	case c.Credentials != nil:
		options = append(options, option.WithCredentials(c.Credentials))
	default:
		return nil, errors.New("no credentials provided")
	}

	client, err := bigquery.NewClient(
		context.Background(),
		c.ProjectID,
		options...,
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create bigquery client")
	}

	if c.Location != "" {
		client.Location = c.Location
	}

	return &Client{
		client: client,
		config: c,
	}, nil
}

func (d *Client) GetIngestrURI() (string, error) {
	return d.config.GetIngestrURI()
}

func (d *Client) IsValid(ctx context.Context, query *query.Query) (bool, error) {
	q := d.client.Query(query.ToDryRunQuery())
	q.DryRun = true

	job, err := q.Run(ctx)
	if err != nil {
		return false, formatError(err)
	}

	status := job.LastStatus()
	if err := status.Err(); err != nil {
		return false, err
	}

	return true, nil
}

func (d *Client) RunQueryWithoutResult(ctx context.Context, query *query.Query) error {
	q := d.client.Query(query.String())
	_, err := q.Read(ctx)
	if err != nil {
		return formatError(err)
	}

	return nil
}

func (d *Client) Select(ctx context.Context, query *query.Query) ([][]interface{}, error) {
	q := d.client.Query(query.String())
	rows, err := q.Read(ctx)
	if err != nil {
		return nil, formatError(err)
	}

	result := make([][]interface{}, 0)
	for {
		var values []bigquery.Value
		err := rows.Next(&values)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}

		interfaces := make([]interface{}, len(values))
		for i, v := range values {
			interfaces[i] = v
		}

		result = append(result, interfaces)
	}

	return result, nil
}

func (d *Client) SelectWithSchema(ctx context.Context, queryObj *query.Query) (*query.QueryResult, error) {
	q := d.client.Query(queryObj.String())
	rows, err := q.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initiate query read: %w", err)
	}

	result := &query.QueryResult{
		Columns:     []string{},
		Rows:        [][]interface{}{},
		ColumnTypes: []string{},
	}

	// Add a ColumnTypes field to store the types
	columnTypes := []string{}

	for {
		var values []bigquery.Value
		err := rows.Next(&values)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read row: %w", err)
		}

		row := make([]interface{}, len(values))
		for i, v := range values {
			row[i] = v
		}
		result.Rows = append(result.Rows, row)
	}

	if rows.Schema != nil {
		for _, field := range rows.Schema {
			result.Columns = append(result.Columns, field.Name)
			// Extract the type information from the schema
			columnTypes = append(columnTypes, string(field.Type))
		}
	} else {
		return nil, errors.New("schema information is not available")
	}

	// Store the column types in the result
	result.ColumnTypes = columnTypes

	return result, nil
}

type NoMetadataUpdatedError struct{}

func (m NoMetadataUpdatedError) Error() string {
	return "no metadata found for the given asset to be pushed to BigQuery"
}

func (d *Client) getTableRef(tableName string) (*bigquery.Table, error) {
	tableComponents := strings.Split(tableName, ".")
	// Check for empty components
	for _, component := range tableComponents {
		if component == "" {
			return nil, fmt.Errorf("table name must be in dataset.table or project.dataset.table format, '%s' given", tableName)
		}
	}
	switch len(tableComponents) {
	case 2:
		return d.client.DatasetInProject(d.config.ProjectID, tableComponents[0]).Table(tableComponents[1]), nil
	case 3:
		return d.client.DatasetInProject(tableComponents[0], tableComponents[1]).Table(tableComponents[2]), nil
	default:
		return nil, fmt.Errorf("table name must be in dataset.table or project.dataset.table format, '%s' given", tableName)
	}
}

func (d *Client) UpdateTableMetadataIfNotExist(ctx context.Context, asset *pipeline.Asset) error {
	anyColumnHasDescription := false
	colsByName := make(map[string]*pipeline.Column, len(asset.Columns))
	for _, col := range asset.Columns {
		colsByName[col.Name] = &col
		if col.Description != "" {
			anyColumnHasDescription = true
		}
	}

	if asset.Description == "" && (len(asset.Columns) == 0 || !anyColumnHasDescription) {
		return NoMetadataUpdatedError{}
	}
	tableRef, err := d.getTableRef(asset.Name)
	if err != nil {
		return err
	}

	meta, err := tableRef.Metadata(ctx)
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == 404 {
			return nil
		}
		return err
	}
	schema := meta.Schema
	colsChanged := false
	for _, field := range schema {
		if col, ok := colsByName[field.Name]; ok {
			field.Description = col.Description
			colsChanged = true
		}
	}

	update := bigquery.TableMetadataToUpdate{}

	if colsChanged {
		update.Schema = schema
	}

	if asset.Description != "" {
		update.Description = asset.Description
	}
	primaryKeys := asset.ColumnNamesWithPrimaryKey()
	if len(primaryKeys) > 0 {
		update.TableConstraints = &bigquery.TableConstraints{
			PrimaryKey: &bigquery.PrimaryKey{Columns: primaryKeys},
		}
	}

	if _, err = tableRef.Update(ctx, update, meta.ETag); err != nil {
		return errors.Wrap(err, "failed to update table metadata")
	}

	return nil
}

func formatError(err error) error {
	var googleError *googleapi.Error
	if !errors.As(err, &googleError) {
		return err
	}

	if googleError.Code == 404 || googleError.Code == 400 {
		return fmt.Errorf("%s", googleError.Message)
	}

	return googleError
}

// Test runs a simple query (SELECT 1) to validate the connection.
func (d *Client) Ping(ctx context.Context) error {
	// Define the test query
	q := query.Query{
		Query: "SELECT 1",
	}

	// Use the existing RunQueryWithoutResult method
	err := d.RunQueryWithoutResult(ctx, &q)
	if err != nil {
		return errors.Wrap(err, "failed to run test query on Bigquery connection")
	}

	return nil // Return nil if the query runs successfully
}

func (d *Client) IsPartitioningOrClusteringMismatch(ctx context.Context, meta *bigquery.TableMetadata, asset *pipeline.Asset) bool {
	if meta.TimePartitioning != nil || meta.RangePartitioning != nil || asset.Materialization.PartitionBy != "" || len(asset.Materialization.ClusterBy) > 0 {
		if !IsSamePartitioning(meta, asset) || !IsSameClustering(meta, asset) {
			return true
		}
	}
	return false
}

func IsSamePartitioning(meta *bigquery.TableMetadata, asset *pipeline.Asset) bool {
	if asset.Materialization.PartitionBy != "" &&
		meta.TimePartitioning == nil &&
		meta.RangePartitioning == nil {
		return false
	}

	if meta.TimePartitioning == nil && meta.RangePartitioning == nil {
		return true
	}

	if meta.TimePartitioning != nil {
		if meta.TimePartitioning.Field != asset.Materialization.PartitionBy {
			return false
		}
	}
	if meta.RangePartitioning != nil {
		if meta.RangePartitioning.Field != asset.Materialization.PartitionBy {
			return false
		}
	}
	return true
}

func IsSameClustering(meta *bigquery.TableMetadata, asset *pipeline.Asset) bool {
	if len(asset.Materialization.ClusterBy) > 0 &&
		(meta.Clustering == nil || len(meta.Clustering.Fields) == 0) {
		return false
	}
	if meta.Clustering == nil {
		return true
	}

	bigQueryFields := meta.Clustering.Fields
	userFields := asset.Materialization.ClusterBy

	if len(bigQueryFields) != len(userFields) {
		return false
	}

	for i := range bigQueryFields {
		if bigQueryFields[i] != userFields[i] {
			return false
		}
	}

	return true
}

func (d *Client) CreateDataSetIfNotExist(asset *pipeline.Asset, ctx context.Context) error {
	tableName := asset.Name
	tableComponents := strings.Split(tableName, ".")
	var datasetName string
	var projectID string

	switch len(tableComponents) {
	case 2:
		projectID = d.config.ProjectID
		datasetName = tableComponents[0]
	case 3:
		datasetName = tableComponents[1]
		projectID = tableComponents[0]
	default:
		return nil
	}

	cacheKey := fmt.Sprintf("%s.%s", projectID, datasetName)

	if _, exists := datasetNameCache.Load(cacheKey); exists {
		return nil
	}

	lock, _ := datasetLocks.LoadOrStore(cacheKey, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)

	mutex.Lock()
	defer mutex.Unlock()

	if _, exists := datasetNameCache.Load(cacheKey); exists {
		return nil
	}

	dataset := d.client.DatasetInProject(projectID, datasetName)
	_, err := dataset.Metadata(ctx)
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == 404 {
			if err := dataset.Create(ctx, &bigquery.DatasetMetadata{}); err != nil {
				return fmt.Errorf("failed to create dataset '%s': %w", datasetName, err)
			}
			datasetNameCache.Store(cacheKey, true)
		} else {
			return fmt.Errorf("failed to fetch metadata for table '%s': %w", tableName, err)
		}
	}

	return nil
}

func (d *Client) IsMaterializationTypeMismatch(ctx context.Context, meta *bigquery.TableMetadata, asset *pipeline.Asset) bool {
	if asset.Materialization.Type == pipeline.MaterializationTypeNone {
		return false
	}

	tableType := meta.Type
	return !strings.EqualFold(string(tableType), string(asset.Materialization.Type))
}

func (d *Client) DropTableOnMismatch(ctx context.Context, tableName string, asset *pipeline.Asset) error {
	tableRef, err := d.getTableRef(tableName)
	if err != nil {
		return err
	}
	meta, err := tableRef.Metadata(ctx)
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == 404 {
			return nil
		}
		return fmt.Errorf("failed to fetch metadata for table '%s': %w", tableName, err)
	}
	if d.IsMaterializationTypeMismatch(ctx, meta, asset) || d.IsPartitioningOrClusteringMismatch(ctx, meta, asset) {
		if err := tableRef.Delete(ctx); err != nil {
			return fmt.Errorf("failed to delete table '%s': %w", tableName, err)
		}
	}
	return nil
}

func (d *Client) BuildTableExistsQuery(tableName string) (string, error) {
	tableComponents := strings.Split(tableName, ".")
	for _, component := range tableComponents {
		if component == "" {
			return "", fmt.Errorf("table name must be in dataset.table or project.dataset.table format, '%s' given", tableName)
		}
	}

	var datasetRef, targetTable string

	switch len(tableComponents) {
	case 2:
		datasetRef = fmt.Sprintf("%s.%s.INFORMATION_SCHEMA.TABLES", d.config.ProjectID, tableComponents[0])
		targetTable = tableComponents[1]
	case 3:
		datasetRef = fmt.Sprintf("%s.%s.INFORMATION_SCHEMA.TABLES", tableComponents[0], tableComponents[1])
		targetTable = tableComponents[2]
	default:
		return "", fmt.Errorf("table name must be in dataset.table or project.dataset.table format, '%s' given", tableName)
	}

	// Use EXISTS to return true or false
	query := fmt.Sprintf(`
		SELECT EXISTS (SELECT 1 FROM %s WHERE table_name = '%s')`, datasetRef, targetTable)

	return strings.TrimSpace(query), nil
}
