package topology

import "testing"

// Central binds a mutual-TLS listener only in a release whose compose file
// sets MPORT for it; MutualTLSPort holds VersolaApp's default otherwise,
// which says nothing about central.
func TestCentralMutualTLSSet(t *testing.T) {
	topo := mustParse(t, `{"services":{
	  "none":{"network_mode":"host","environment":{"PORT":"8090","DPORT":"8091"}},
	  "set":{"network_mode":"host","environment":{"PORT":"8090","DPORT":"8091","MPORT":"8093"}},
	  "numeric":{"network_mode":"host","environment":{"MPORT":8093}},
	  "empty":{"network_mode":"host","environment":{"MPORT":""}},
	  "bad":{"network_mode":"host","environment":{"MPORT":"x"}}}}`)
	for name, want := range map[string]bool{"none": false, "set": true, "numeric": true, "empty": false, "bad": false} {
		s, err := topo.Service(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.MutualTLSSet() != want {
			t.Errorf("%s: MutualTLSSet %v, want %v", name, s.MutualTLSSet(), want)
		}
	}
	s := mustService(t, topo, "set")
	if s.MutualTLSPort != 8093 {
		t.Errorf("MPORT %d", s.MutualTLSPort)
	}
	// A slot keeps it, shifted like the other ports.
	slot, err := s.ForSlot(2)
	if err != nil {
		t.Fatal(err)
	}
	if !slot.MutualTLSSet() || slot.MutualTLSPort != 8193 {
		t.Errorf("slot 2: %v %d", slot.MutualTLSSet(), slot.MutualTLSPort)
	}
}
