package main

import (
	"path/filepath"
	"testing"
)

func TestServiceKeepsForkRuntimeSettings(t *testing.T) {
	t.Setenv("PAIRFOB_AUTO_UPDATE", "0")
	t.Setenv("PAIRFOB_MERGE_SESSIONS", "1")
	t.Setenv("PAIRFOB_ALLINONE_CURSOR_CONFIG", "relative/cursor.json")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7897")
	t.Setenv("http_proxy", "http://127.0.0.1:7897")
	t.Setenv("NO_PROXY", "localhost,127.0.0.1,::1")
	env := map[string]string{}
	for _, entry := range serviceRuntimeEnvironment() {
		env[entry[0]] = entry[1]
	}
	absolute, err := filepath.Abs("relative/cursor.json")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"PAIRFOB_AUTO_UPDATE": "0", "PAIRFOB_MERGE_SESSIONS": "1", "PAIRFOB_ALLINONE_CURSOR_CONFIG": absolute,
		// Proxy settings are URLs and host lists, never turned into paths.
		"HTTPS_PROXY": "http://127.0.0.1:7897", "http_proxy": "http://127.0.0.1:7897", "NO_PROXY": "localhost,127.0.0.1,::1",
	} {
		if env[key] != want {
			t.Fatalf("%s = %q, want %q (env %v)", key, env[key], want, env)
		}
	}
}

func TestServiceNeverPersistsAProxyCredential(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://user:secret@proxy.example:8080")
	t.Setenv("HTTP_PROXY", "http://proxy.example:8080")
	env := map[string]string{}
	for _, entry := range serviceRuntimeEnvironment() {
		env[entry[0]] = entry[1]
	}
	if _, ok := env["HTTPS_PROXY"]; ok {
		t.Fatalf("a proxy URL with a credential was persisted: %v", env)
	}
	if env["HTTP_PROXY"] != "http://proxy.example:8080" {
		t.Fatalf("a plain proxy must persist: %v", env)
	}
	for value, want := range map[string]bool{
		"http://127.0.0.1:7897": false, "localhost,127.0.0.1": false,
		"http://u:p@h:1": true, "http://u@h:1": true, "%zz@h": true,
	} {
		if got := proxyCarriesCredential(value); got != want {
			t.Fatalf("proxyCarriesCredential(%q) = %v, want %v", value, got, want)
		}
	}
}
