package server

import (
	"net/http"

	"github.com/Trones21/bluestaq-takehome/api"
	"github.com/go-chi/chi/v5"
)

// Docs serve the hand-written OpenAPI document and a Swagger UI over it.
//
// There is no frontend in this project -- it is a backend challenge -- so this
// is the substitute: a reviewer needs some way to exercise the API without
// hand-writing curl for a two-phase upload or a version conflict.
//
// The spec is served from an embedded file rather than generated from handler
// annotations, because generating it makes the implementation the contract:
// whatever the code happens to do becomes what the API "promises", so a
// regression quietly regenerates into the document.
func docsRoutes(r chi.Router) {
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(api.OpenAPI)
	})
	r.Get("/docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(swaggerUI)
	})
}

// Swagger UI from a CDN. Fine for a local review tool; a real deployment would
// vendor the assets so the docs page does not depend on a third party being up
// and does not need a script-src exception in a content security policy.
var swaggerUI = []byte(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Notes API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>
    body { margin: 0 }
    .topbar { display: none }
    .swagger-ui .info { margin: 24px 0 }
  </style>
</head>
<body>
  <div id="ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: '/openapi.yaml',
      dom_id: '#ui',
      deepLinking: true,
      // Ordered so auth is first: nothing else works until you have a token.
      tagsSorter: (a, b) => ['auth','users','teams','notes'].indexOf(a)
                          - ['auth','users','teams','notes'].indexOf(b),
      persistAuthorization: true,
      tryItOutEnabled: true,
      docExpansion: 'list',
    });
  </script>
</body>
</html>
`)
