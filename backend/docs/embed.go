// Package docs provides the embedded OpenAPI specification.
package docs

import _ "embed"

// SwaggerYAML contains the embedded OpenAPI 3.0 specification.
//
//go:embed swagger.yaml
var SwaggerYAML []byte
