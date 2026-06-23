package config

import (
	"testing"
)

func TestHostRegex(t *testing.T) {
	tests := []struct {
		template string
		header   string
		wantOK   bool
		wantSub  string
		wantHost string
	}{
		{
			template: "${subdomain}${host}.local",
			header:   "api.main.local",
			wantOK:   true,
			wantSub:  "api",
			wantHost: "main",
		},
		{
			template: "${subdomain}${host}.local",
			header:   "main.local",
			wantOK:   true,
			wantSub:  "",
			wantHost: "main",
		},
	}

	for _, tt := range tests {
		re, err := compileTemplate(tt.template)
		if err != nil {
			t.Fatalf("compileTemplate(%q): %v", tt.template, err)
		}

		matches := re.FindStringSubmatch(tt.header)
		ok := matches != nil

		if ok != tt.wantOK {
			t.Errorf("template=%q header=%q: got match=%v, want %v", tt.template, tt.header, ok, tt.wantOK)
			continue
		}

		if !ok {
			continue
		}

		subIdx := re.SubexpIndex("subdomain")
		hostIdx := re.SubexpIndex("host")

		gotSub := ""
		gotHost := ""
		if subIdx >= 0 {
			gotSub = matches[subIdx]
		}
		if hostIdx >= 0 {
			gotHost = matches[hostIdx]
		}

		if gotSub != tt.wantSub {
			t.Errorf("template=%q header=%q: subdomain=%q, want %q", tt.template, tt.header, gotSub, tt.wantSub)
		}
		if gotHost != tt.wantHost {
			t.Errorf("template=%q header=%q: host=%q, want %q", tt.template, tt.header, gotHost, tt.wantHost)
		}
	}
}
