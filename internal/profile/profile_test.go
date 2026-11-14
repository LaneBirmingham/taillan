package profile

import (
	"strings"
	"testing"
)

func TestBuiltinsValid(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err) // NewStore validates every built-in
	}
	all := s.All()
	if len(all) < 15 {
		t.Fatalf("only %d built-in games", len(all))
	}
	for _, p := range all {
		if len(p.Host.Steps) == 0 || len(p.Join.Steps) == 0 {
			t.Errorf("%s: needs host and join steps", p.ID)
		}
		joined := strings.Join(p.Join.Steps, " ")
		if !strings.Contains(joined, "{{connect}}") && !strings.Contains(joined, "{{host}}") {
			t.Errorf("%s: join steps never tell the player what to connect to", p.ID)
		}
	}
}

func TestParsePorts(t *testing.T) {
	tests := []struct {
		in   string
		want string
		bad  bool
	}{
		{in: "25565", want: "25565/both"},
		{in: "7777/udp", want: "7777/udp"},
		{in: "2456-2457/u, 27015/t", want: "2456-2457/udp, 27015/tcp"},
		{in: "  3979/both ;8080 ", want: "3979/both, 8080/both"},
		{in: "70000", bad: true},
		{in: "10-5/udp", bad: true},
		{in: "80/sctp", bad: true},
		{in: "", bad: true},
	}
	for _, tt := range tests {
		ports, err := ParsePorts(tt.in)
		if tt.bad {
			if err == nil {
				t.Errorf("ParsePorts(%q) = %v, want error", tt.in, ports)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePorts(%q): %v", tt.in, err)
			continue
		}
		p := &Profile{Ports: ports}
		if got := p.PortSummary(); got != tt.want {
			t.Errorf("ParsePorts(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestValidate(t *testing.T) {
	bad := []Profile{
		{ID: "Bad ID", Name: "x", Ports: []Port{{Port: 1, Proto: "tcp"}}},
		{ID: "x", Name: "", Ports: []Port{{Port: 1, Proto: "tcp"}}},
		{ID: "x", Name: "x"},
		{ID: "x", Name: "x", Ports: []Port{{Port: ControlPort, Proto: "tcp"}}},
		{ID: "x", Name: "x", Ports: []Port{{Port: 1000, End: 2000, Proto: "udp"}}}, // too many
		{ID: "x", Name: "x", Ports: []Port{{Port: 7777, Proto: "both"}, {Port: 7777, Proto: "udp"}}},
	}
	for i, p := range bad {
		if p.Validate() == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
	if _, err := Parse([]byte(`{"id":"x","name":"X","ports":[{"port":1,"prot":"tcp"}]}`)); err == nil {
		t.Error("typo'd field accepted")
	}
}

func TestMappings(t *testing.T) {
	p := &Profile{Ports: []Port{{Port: 2456, End: 2457, Proto: "udp"}, {Port: 27015, Proto: "both"}}}
	m := p.Mappings()
	if len(m) != 4 || m[1].Port != 2457 || m[2].Proto != "tcp" || m[3].Group != 1 {
		t.Errorf("Mappings = %+v", m)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Don't Starve Together": "dont-starve-together",
		"  Space   Haven!! ":    "space-haven",
		"???":                   "custom-game",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUserOverrides(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &Profile{ID: "terraria", Name: "Terraria (my server)", Ports: []Port{{Port: 7778, Proto: "tcp"}}}
	if _, err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if got := s.Get("terraria"); got.Name != "Terraria (my server)" || got.Source != "user" {
		t.Errorf("user file didn't override built-in: %+v", got)
	}
	if b := s.Builtin("terraria"); b.Ports[0].Port != 7777 {
		t.Error("built-in changed")
	}
	if err := s.Delete("terraria"); err != nil {
		t.Fatal(err)
	}
	if s.Get("terraria").Source != "builtin" {
		t.Error("delete didn't restore built-in")
	}
	if err := s.Delete("factorio"); err == nil {
		t.Error("deleted a built-in")
	}
}
