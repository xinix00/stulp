package webapi

import "testing"

// Een pagina die het pad van de brug alleen noemt, hoort de brug wél te krijgen.
// Zonder tag geen Stulp, en dan meldt het eigen script van de pagina dat.
func TestBridgeInjectionLooksForARealScriptTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		html string
		has  bool
	}{
		{"kale pagina", `<p>hallo</p>`, false},
		{"eigen brug", `<script src="/stulp.js"></script><p>hallo</p>`, true},
		{"eigen brug, relatief", `<script defer src="../stulp.js"></script>`, true},
		{"alleen genoemd", `<script>fetch('/stulp.js');</script>`, false},
		{"in commentaar", `<!-- /stulp.js hoort hier niet --><p>hallo</p>`, false},
	}
	for _, test := range tests {
		if got := hasBridgeScript(test.html); got != test.has {
			t.Errorf("%s: hasBridgeScript = %t, wilde %t", test.name, got, test.has)
		}
	}
}
