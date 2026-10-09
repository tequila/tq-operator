`rendered-spec-dev.json` is the exact Estate spec `tq` renders for a two-service fixture.
`rendered-spec-prod.json` is its prod variant: `cloud: aws`, `mongodb_in_cluster: false`, a
declared backup, and the operator on.
The Go types round-trip both byte for byte; the published schema and the API server accept both.
