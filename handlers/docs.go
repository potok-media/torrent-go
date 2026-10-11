package handlers

import "net/http"

// docsPage is a self-contained Scalar API-reference page pointed at the embedded OpenAPI spec
// (/openapi.yaml). The Scalar runtime comes from the jsDelivr CDN; the page itself carries no assets.
const docsPage = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Potok TorrentGo API</title>
  </head>
  <body>
    <script
      id="api-reference"
      data-url="/openapi.yaml"
      data-configuration='{"title":"Potok TorrentGo API"}'
    ></script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
  </body>
</html>
`

// HandleDocs godoc
//	@ID			getApiDocs
//
//	@Summary		Interactive API documentation (Scalar UI)
//	@Description	Serves the Scalar API-reference page rendering /openapi.yaml.
//	@Tags			System
//	@Produce		html
//	@Success		200	{string}	string	"Scalar HTML page"
//	@Security
//	@Router			/docs [get]
func HandleDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(docsPage))
}
