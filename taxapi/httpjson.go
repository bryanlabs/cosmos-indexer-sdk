package taxapi

import (
	"net/http"
	"net/url"

	"github.com/DefiantLabs/cosmos-indexer/taxapi/internal/servicehttp"
)

func configuredJSONGet(client *http.Client, origin, path string, query url.Values, out any) error {
	endpoint, err := servicehttp.New(client, origin)
	if err != nil {
		return err
	}
	return endpoint.GetJSON(path, query, out)
}
