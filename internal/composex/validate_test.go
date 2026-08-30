package composex

import (
	"strings"
	"testing"
)

// The refusals are a security boundary, so the table is the specification: every
// key named in the analysis is refused, and named in the message so the user
// knows what to change.
func TestCheckRefusesWhatWouldLeaveTheContainer(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"privileged", `{"services":{"db":{"privileged":true}}}`, "privileged"},
		{"cap_add", `{"services":{"db":{"cap_add":["SYS_ADMIN"]}}}`, "cap_add"},
		{"security_opt", `{"services":{"db":{"security_opt":["seccomp=unconfined"]}}}`, "security_opt"},
		{"devices", `{"services":{"db":{"devices":["/dev/kvm:/dev/kvm"]}}}`, "devices"},
		{"host networking", `{"services":{"db":{"network_mode":"host"}}}`, "network_mode: host"},
		{"the host pid namespace", `{"services":{"db":{"pid":"host"}}}`, "pid: host"},
		{"the host ipc namespace", `{"services":{"db":{"ipc":"host"}}}`, "ipc: host"},
		{"the host uts namespace", `{"services":{"db":{"uts":"host"}}}`, "uts: host"},
		{"user root", `{"services":{"db":{"user":"root"}}}`, "user root"},
		{"user 0", `{"services":{"db":{"user":"0"}}}`, "user 0"},
		{"user 0:0", `{"services":{"db":{"user":"0:0"}}}`, "user 0:0"},
		{"user root:root", `{"services":{"db":{"user":"root:root"}}}`, "user root:root"},
		{"a host bind mount", `{"services":{"db":{"volumes":[{"type":"bind","source":"/","target":"/host"}]}}}`, "/ in volumes"},
		{"the docker socket", `{"services":{"db":{"volumes":[{"type":"bind","source":"/var/run/docker.sock","target":"/var/run/docker.sock"}]}}}`, "docker.sock"},
		{"build", `{"services":{"db":{"build":{"context":"."}}}}`, "build"},
		{"a fixed host port", `{"services":{"db":{"ports":[{"target":5432,"published":"5432"}]}}}`, "fixed host port on 5432"},
		{"a fixed host port as a number", `{"services":{"db":{"ports":[{"target":5432,"published":5432}]}}}`, "fixed host port on 5432"},
		{"Hexagon's own service name", `{"services":{"hexagon":{"image":"busybox"}}}`, "Hexagon's own service"},
		{"no services at all", `{"services":{}}`, "no services"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := check([]byte(c.doc))
			if err == nil {
				t.Fatalf("check accepted %s", c.doc)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to name %q", err, c.want)
			}
		})
	}
}

// The long and the short form of a key are the same request, and both arrive
// here already normalized: that is the whole reason validation happens over the
// CLI's own output rather than over the text the user typed.
func TestCheckAcceptsAnOrdinaryFile(t *testing.T) {
	const doc = `{"services":{
		"cache":{"image":"redis:7","user":"999:999"},
		"db":{"image":"postgres:16","user":"999",
		      "volumes":[{"type":"volume","source":"data","target":"/var/lib/postgresql/data"},
		                 {"type":"tmpfs","target":"/tmp"}],
		      "ports":[{"mode":"ingress","target":5432,"protocol":"tcp"}],
		      "cap_add":[],"security_opt":null,"build":null,"published":""}
	}}`
	services, err := check([]byte(doc))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(services) != 2 || services[0] != "cache" || services[1] != "db" {
		t.Errorf("services = %v, want [cache db]", services)
	}
}

// A port with no host side is the form that works: Docker picks the host port,
// so a second session of the same project does not fail to start.
func TestCheckAcceptsAPortWithNoHostSide(t *testing.T) {
	for _, doc := range []string{
		`{"services":{"db":{"ports":[{"target":5432}]}}}`,
		`{"services":{"db":{"ports":[{"target":5432,"published":""}]}}}`,
		`{"services":{"db":{"ports":[{"target":5432,"published":null}]}}}`,
	} {
		if _, err := check([]byte(doc)); err != nil {
			t.Errorf("check(%s) = %v, want it accepted", doc, err)
		}
	}
}

// A document the CLI answered with in a shape this package does not understand
// is refused rather than waved through: this is a denylist, and one that cannot
// read the document has nothing to deny.
func TestCheckRefusesSomethingItCannotRead(t *testing.T) {
	if _, err := check([]byte(`not json`)); err == nil {
		t.Error("check accepted something that is not a normalized compose document")
	}
}
