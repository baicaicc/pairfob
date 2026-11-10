package main

import (
	"net/url"
	"strings"
)

// forkServiceKeys are the runtime settings this fork's deployments rely on,
// so `pairfob service install` keeps them (docs/kidsbox.md): no self-update
// to the hosted channel, merged Herdr sessions, the All in One Cursor
// registry, and the proxy the relay connection may need.
var forkServiceKeys = []string{
	"PAIRFOB_AUTO_UPDATE", "PAIRFOB_MERGE_SESSIONS", "PAIRFOB_ALLINONE_CURSOR_CONFIG",
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy",
}

func serviceProxyKey(key string) bool {
	switch strings.ToUpper(key) {
	case "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY":
		return true
	}
	return false
}

// proxyCarriesCredential reports a proxy URL with user information. Persisted
// service settings may be printed, so such a value is never written; the
// operator sets it in the unit by hand.
func proxyCarriesCredential(value string) bool {
	if strings.Contains(value, "@") {
		parsed, err := url.Parse(value)
		return err != nil || parsed.User != nil || strings.Contains(parsed.Host, "@")
	}
	return false
}
