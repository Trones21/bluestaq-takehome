// Package api embeds the OpenAPI document so the binary carries its own
// contract -- a deployed service and its documentation cannot drift apart or
// be deployed separately.
package api

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte
