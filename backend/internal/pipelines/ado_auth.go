// ado_auth.go encapsulates the two Azure DevOps HTTP auth schemes so callers can
// pass a credential without knowing how it is presented on the wire: a personal
// access token authenticates with Basic (":"+pat); a Microsoft Entra app
// access token (minted via client-credentials) uses Bearer.
package pipelines

import (
	"encoding/base64"
	"net/http"
)

// ADOToken is an Azure DevOps credential plus the auth scheme it requires.
type ADOToken struct {
	Value  string
	Bearer bool // true for Entra app access tokens; false (default) for PATs
}

// ADOPAT wraps a personal access token (Basic auth) — the historical default.
func ADOPAT(pat string) ADOToken { return ADOToken{Value: pat} }

// ADOBearer wraps an Entra app access token (Bearer auth).
func ADOBearer(token string) ADOToken { return ADOToken{Value: token, Bearer: true} }

// empty reports whether no credential value is set.
func (t ADOToken) empty() bool { return t.Value == "" }

// authorize sets the Authorization header for the chosen scheme.
func (t ADOToken) authorize(req *http.Request) {
	if t.Bearer {
		req.Header.Set("Authorization", "Bearer "+t.Value)
		return
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(":"+t.Value)))
}
