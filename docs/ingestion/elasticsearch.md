# Elasticsearch
[Elasticsearch](https://www.elastic.co/elasticsearch) is a distributed, RESTful search and analytics engine designed for fast and scalable data retrieval and storage.

Bruin supports Elasticsearch as a source for [Ingestr assets](/assets/ingestr), and you can use it to ingest data from Elasticsearch into your data warehouse.

In order to set up Elasticsearch connection, you need to add a configuration item in the `.bruin.yml` file and in `asset` file.

Follow the steps below to correctly set up Elasticsearch as a data source and run ingestion.

### Step 1: Add a connection to .bruin.yml file
To connect to Elasticsearch, you need to add a configuration item to the connections section of the `.bruin.yml` file. This configuration must comply with the following schema:

```yaml
  elasticsearch:
    - name: "elasticsearch"
      username: "username_123"
      password: "pass_123"
      host: "localhost"
      port: 5000
      secure: "false"
      verify_certs: "false"
```
- `username`: The username used to authenticate with Elasticsearch.
- `password`: The password associated with the specified username.
- `host`: The host address of the Elasticsearch server.
- `port`: The port number used by the Elasticsearch server.
-  `secure`: Enables HTTPS when set to true. By default, it is true.
- `verify_certs`: Verifies TLS certificates when set to true. By default, it is true.

### Step 2: Create an asset file for data ingestion
To ingest data from Elasticsearch, you need to create an [asset configuration](/assets/ingestr#asset-structure) file. This file defines the data flow from the source to the destination. Create a YAML file (e.g., Elasticsearch_ingestion.yml) inside the assets folder and add the following content:

```yaml
name: public.elasticsearch
type: ingestr
connection: postgres

parameters:
  source_connection: elasticsearch
  source_table: 'index-name'

  destination: postgres
```

- `name`: The name of the asset.
- `type`: Specifies the type of the asset. Set this to ingestr to use the ingestr data pipeline.
- `connection`: This is the destination connection, which defines where the data should be stored. For example: `postgres` indicates that the ingested data will be stored in a Postgres database.
- `source_connection`: The name of the Elasticsearch connection defined in .bruin.yml.
- `source_table`: The name of the Elasticsearch index from which you want to ingest data.

### Step 3: [Run](/commands/run) asset to ingest data
```     
bruin run assets/elasticsearch_ingestion.yml
```
As a result of this command, Bruin will ingest data from the given Elasticsearch table into your Postgres database.
