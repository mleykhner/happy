package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLegacyClientMigration(t *testing.T) {
	s, _ := fixture(t)
	old := selectedState(t)
	old.Settings.Endpoint = "vpn.example"
	before, err := old.clientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.store.save(old); err != nil {
		t.Fatal(err)
	}
	migrated, err := NewService(s.store, s.xray)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.state.Client != nil || len(migrated.state.Clients) != 1 || migrated.state.Clients[0].ID == "" {
		t.Fatal("legacy client not migrated")
	}
	after, err := migrated.ClientConfig(migrated.state.Clients[0].ID)
	if err != nil || before != after {
		t.Fatal("migration changed existing config", err)
	}
	restarted, err := NewService(s.store, s.xray)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.state.Clients) != 1 || restarted.state.Clients[0].ID != migrated.state.Clients[0].ID {
		t.Fatal("migration is not stable")
	}
}

func TestMultipleClientsLifecycle(t *testing.T) {
	s, _ := fixture(t)
	s.state.Settings.Endpoint = "vpn.example"
	for _, name := range []string{"Телефон", "Ноутбук"} {
		if err := s.changeClient(t.Context(), "", "create", name); err != nil {
			t.Fatal(err)
		}
	}
	first, second := s.state.Clients[0], s.state.Clients[1]
	if first.Address != 2 || second.Address != 3 || first.PublicKey == second.PublicKey || first.ID == second.ID {
		t.Fatal("clients must have independent identities and addresses")
	}
	secondConfig, err := s.ClientConfig(second.ID)
	if err != nil || !strings.Contains(secondConfig, "Address = 10.77.0.3/32, fd77::3/128") || strings.Contains(secondConfig, first.PrivateKey) {
		t.Fatal("wrong client config", err)
	}
	if _, err = s.ClientConfig("missing"); err == nil {
		t.Fatal("unknown client config returned")
	}
	view, _ := json.Marshal(s.View())
	for _, c := range s.state.Clients {
		if strings.Contains(string(view), c.PrivateKey) {
			t.Fatal("private key leaked")
		}
	}
	if err = s.changeClient(t.Context(), first.ID, "disable", ""); err != nil {
		t.Fatal(err)
	}
	config, err := BuildConfig(s.state, 18080, "test")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), first.PublicKey) || !strings.Contains(string(config), second.PublicKey) {
		t.Fatal("disable affected wrong peer")
	}
	if err = s.changeClient(t.Context(), first.ID, "rotate", ""); err != nil {
		t.Fatal(err)
	}
	if s.state.Clients[0].PublicKey == first.PublicKey || s.state.Clients[0].Enabled || s.state.Clients[0].Address != first.Address || s.state.Clients[1] != second {
		t.Fatal("rotation changed another client or address/status")
	}
	if err = s.changeClient(t.Context(), first.ID, "enable", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.changeClient(t.Context(), first.ID, "delete", ""); err != nil {
		t.Fatal(err)
	}
	if len(s.state.Clients) != 1 || s.state.Clients[0] != second {
		t.Fatal("delete affected another peer")
	}
	if err = s.changeClient(t.Context(), "", "create", "Планшет"); err != nil {
		t.Fatal(err)
	}
	if s.state.Clients[1].Address != 2 || s.state.Clients[1].PublicKey == first.PublicKey {
		t.Fatal("address not safely reused with fresh keys")
	}
	restored, err := NewService(s.store, s.xray)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.state.Clients) != 2 || restored.state.Clients[0] != second {
		t.Fatal("clients not persisted")
	}
}

func TestClientAddressPoolIncludesDisabledClients(t *testing.T) {
	s, _ := fixture(t)
	for address := 2; address <= 254; address++ {
		s.state.Clients = append(s.state.Clients, Client{ID: randomToken(), Address: address})
	}
	if err := s.changeClient(t.Context(), "", "create", "Overflow"); err == nil {
		t.Fatal("allocated occupied address")
	}
	if len(s.state.Clients) != 253 {
		t.Fatal("failed allocation changed state")
	}
}
