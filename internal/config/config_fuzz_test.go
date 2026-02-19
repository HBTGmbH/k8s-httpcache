package config

import "testing"

func FuzzListenAddrSet(f *testing.F) {
	seeds := []string{
		"http=:8080,HTTP",
		"purge=:8081",
		":9090,HTTP",
		"http=0.0.0.0:8080,HTTP",
		"admin=127.0.0.1:8081",
		"http=:8080,HTTP,PROXY",
		":8080",
		"0.0.0.0:8080",
		"http=[::1]:8080",
		"=:8080,HTTP",
		"http=noport",
		"http=:99999",
		"http=:0",
		"http=:abc",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, val string) {
		var lf listenAddrFlags
		lf.Set(val) //nolint:errcheck // fuzz: we only care about panics
	})
}

func FuzzBackendSet(f *testing.F) {
	seeds := []string{
		"api:my-svc",
		"api:my-svc:3000",
		"api:my-svc:http",
		"api:staging/my-svc",
		"api:staging/my-svc:8080",
		"api:staging/my-svc:http",
		"api",
		":my-svc",
		"api:",
		"api:my-svc:",
		"api:my-svc:99999",
		"api:my-svc:0",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, val string) {
		var bf backendFlags
		bf.Set(val) //nolint:errcheck // fuzz: we only care about panics
	})
}

func FuzzParseNamespacedService(f *testing.F) {
	seeds := []struct {
		s         string
		defaultNS string
	}{
		{"my-service", "default"},
		{"staging/my-service", "default"},
		{"/svc", "default"},
		{"ns/", "default"},
		{"", "default"},
		{"ns/svc/extra", "default"},
		{"my-service", ""},
		{"my-service", "kube-system"},
	}
	for _, s := range seeds {
		f.Add(s.s, s.defaultNS)
	}
	f.Fuzz(func(t *testing.T, s, defaultNS string) {
		parseNamespacedService(s, defaultNS) //nolint:errcheck // fuzz: we only care about panics
	})
}
