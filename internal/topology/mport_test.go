package topology

import (
	"strings"
	"testing"
)

// MPORT is read like APORT: from the environment, else VersolaApp's
// default 8083, which auth binds whenever its config has `mutual-tls`.
func TestMutualTLSPort(t *testing.T) {
	topo := mustParse(t, `{"services":{
	  "auth":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8081","APORT":"8082","MPORT":"8083"}},
	  "old":{"network_mode":"host","environment":{"PORT":"8080","DPORT":"8081","APORT":"8082"}},
	  "custom":{"network_mode":"host","environment":{"MPORT":9443}}}}`)
	if got := mustService(t, topo, "auth").MutualTLSPort; got != 8083 {
		t.Errorf("auth: MPORT %d", got)
	}
	// Versola 0.6.2's compose file names no MPORT.
	if got := mustService(t, topo, "old").MutualTLSPort; got != DefaultMutualTLSPort {
		t.Errorf("unset: MPORT %d, want the default %d", got, DefaultMutualTLSPort)
	}
	if got := mustService(t, topo, "custom").MutualTLSPort; got != 9443 {
		t.Errorf("a number: MPORT %d", got)
	}
}

// A bad MPORT fails auth (CheckAdditional), not the configuration, and its
// value is never printed.
func TestBadMPORT(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{"environment":{"MPORT":"hunter2-secret"}}}}`)
	auth, err := topo.Service("auth")
	if err != nil {
		t.Fatalf("auth loads; CheckAdditional is what rejects it: %v", err)
	}
	err = auth.CheckAdditional("auth")
	if err == nil || !strings.Contains(err.Error(), "MPORT") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the value leaked into the error: %v", err)
	}
}

// A slot moves MPORT like the other listeners, and its publication with it.
func TestForSlotShiftsMPORT(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{
	  "environment":{"PORT":"8080","DPORT":"8081","APORT":"8082","MPORT":"8083"},
	  "ports":[{"target":8083,"host_ip":"127.0.0.1","published":"18083"}]}}}`)
	s, err := mustService(t, topo, "auth").ForSlot(3)
	if err != nil {
		t.Fatal(err)
	}
	if s.MutualTLSPort != 8283 {
		t.Errorf("slot 3: MPORT %d, want 8283", s.MutualTLSPort)
	}
	pubs := s.Publications()
	if len(pubs) != 1 || pubs[0].Target != 8283 || pubs[0].Port != 18283 {
		t.Errorf("slot 3: publications %v", pubs)
	}
}

// ShiftAuthListeners moves APORT and MPORT only, and an overflow shows in
// CheckAdditional with the slot it is for.
func TestShiftAuthListeners(t *testing.T) {
	topo := mustParse(t, `{"services":{"auth":{"network_mode":"host",
	  "environment":{"PORT":"8080","DPORT":"8081","APORT":"8082","MPORT":"8083"}}}}`)
	base := mustService(t, topo, "auth")
	slot, err := base.ForSlot(2)
	if err != nil {
		t.Fatal(err)
	}
	s := slot.ShiftAuthListeners(1000, 2)
	if s.Port != 8180 || s.DiagnosticsPort != 8181 || s.AdditionalPort != 9182 || s.MutualTLSPort != 9183 {
		t.Errorf("got %+v", s)
	}
	if err := s.CheckAdditional("auth-2"); err != nil {
		t.Errorf("in range: %v", err)
	}
	high := mustParse(t, `{"services":{"auth":{"network_mode":"host","environment":{"APORT":"64600","MPORT":"64601"}}}}`)
	s = mustService(t, high, "auth").ShiftAuthListeners(1000, 1)
	if err := s.CheckAdditional("auth"); err == nil || !strings.Contains(err.Error(), "slot 1") {
		t.Errorf("out of range: %v", err)
	}
}
