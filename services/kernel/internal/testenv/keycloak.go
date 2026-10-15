package testenv

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// KeycloakToken requests a token from the isolated real IdP. Tests choose the grant, never fake its signature.
func KeycloakToken(t testing.TB, client *http.Client, issuer string, values url.Values) string {
	t.Helper()
	response, err := client.PostForm(issuer+"/protocol/openid-connect/token", values)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var value struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil || value.AccessToken == "" {
		t.Fatalf("Keycloak token status=%d err=%v", response.StatusCode, err)
	}
	return value.AccessToken
}
