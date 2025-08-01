# Design patterns and code conventions

Grafana Mimir adopts some design patterns and code conventions that we ask you to follow when contributing to the project. These conventions have been adopted based on experience gained over time and aim to enforce good coding practices and keep a consistent UX (ie. config).

## Go coding style

Grafana Mimir follows the [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) styleguide and the [Formatting and style](https://peter.bourgon.org/go-in-production/#formatting-and-style) section of Peter Bourgon's [Go: Best Practices for Production Environments](https://peter.bourgon.org/go-in-production/).

Comment sentences should begin with the name of the thing being described and end in a period.

## No global variables

- Do not use global variables

## Prometheus metrics

When registering a metric:

- Do not use a global variable for the metric
- Create and register the metric with `promauto.With(reg)`
- In any internal Grafana Mimir component, do not register the metric to the default Prometheus registerer, but take the registerer in input (ie. `NewComponent(reg prometheus.Registerer)`)

Testing metrics:

- When writing unit tests, test exported metrics using `testutil.GatherAndCompare()`

## Config file and CLI flags conventions

Naming:

- Config file options should be lowercase with words `_` (underscore) separated (ie. `memcached_client`)
- CLI flags should be lowercase with words `-` (dash) separated (ie. `memcached-client`)
- When adding a new config option, look if a similar one already exists within the [config](../configuration/config-file-reference.md) and keep the same naming (ie. `addresses` for a list of network endpoints)

Documentation:

- A CLI flag mentioned in the documentation or changelog should be always prefixed with a single `-` (dash)
