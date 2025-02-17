# APM Integration

The APM integration installs Elasticsearch templates and ingest node pipelines for APM data.

### Quick start

Ready to jump in? Read the [APM quick start](https://ela.st/quick-start-apm).

### How to use this integration

Add the APM integration to an Elastic Agent policy to create an `apm` input.
Any Elastic Agents set up with this policy will run an APM Server binary locally.
Don't forget to configure the APM Server `host` if it needs to be accessed from outside, like when running in Docker.
Then, configure your APM agents to communicate with APM Server.

If you have Real User Monitoring (RUM) enabled, you must run Elastic Agent centrally.
Otherwise, you can run it on edge machines by downloading and installing Elastic Agent
on the same machines that your instrumented services run.

#### Data Streams

When using the APM integration, apm events are indexed into data streams. Data stream names contain the event type,
service name, and a user-configurable namespace.

There is no specific recommendation for what to use as a namespace; it is intentionally flexible.
You might use the environment, like `production`, `testing`, or `development`,
or you could namespace data by business unit. It is your choice.

See [APM data streams](https://ela.st/apm-data-streams) for more information.

## Compatibility and limitations

The APM integration requires Kibana v7.12 and Elasticsearch with at least the basic license.
This version is experimental and has some limitations, listed bellow:

- Sourcemaps need to be uploaded to Elasticsearch directly.
- You need to create specific API keys for sourcemaps and central configuration.
- You can't use an Elastic Agent enrolled before 7.12.
- Not all settings are supported.
- The `apm` templates, pipelines, and ILM settings that ship with this integration cannot be configured or changed with Fleet;
changes must be made with Elasticsearch APIs or Kibana's Stack Management.

See [APM integration limitations](https://ela.st/apm-integration-limitations) for more information.

IMPORTANT: If you run APM Server with Elastic Agent manually in standalone mode, you must install the APM integration before ingestion starts.

## Traces

Traces are comprised of [spans and transactions](https://www.elastic.co/guide/en/apm/get-started/current/apm-data-model.html).

Traces are written to `traces-apm-*` data streams.

{{fields "traces"}}

## Application Metrics

Application metrics are comprised of custom, application-specific metrics, basic system metrics such as CPU and memory usage,
and runtime metrics such as JVM garbage collection statistics.

Application metrics are written to service-specific `metrics-apm.app.*-*` data streams.

{{fields "app_metrics"}}

## Internal Metrics

Internal metrics comprises metrics produced by Elastic APM agents and Elastic APM server for powering various Kibana charts
in the APM app, such as "Time spent by span type".

Internal metrics are written to `metrics-apm.internal-*` data streams.

{{fields "internal_metrics"}}

## Application errors

Application errors comprises error/exception events occurring in an application.

Application errors are written to `logs-apm.error.*` data stream.

{{fields "error_logs"}}
