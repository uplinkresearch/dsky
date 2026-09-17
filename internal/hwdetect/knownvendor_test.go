package hwdetect

import "testing"

// Manufacturer and model as Windows reports them, to the catalog with drivers
// for the machine.
func TestKnownVendor(t *testing.T) {
	for _, c := range []struct{ vendor, model, want string }{
		{"Dell Inc.", "OptiPlex 7010", "dell"},
		{"Alienware", "Alienware m16 R2", "alienware"},
		{"Dell Inc.", "Alienware 17 R4", "alienware"},
		{"LENOVO", "21AH00BUUS", "lenovo"},
		{"HP", "HP EliteBook 840 G10 Notebook PC", "hp"},
		{"Framework", "Laptop 13 (AMD Ryzen 7040Series)", "framework"},
		{"Microsoft Corporation", "Surface Laptop 5", "surface"},
		{"Microsoft Corporation", "Virtual Machine", ""},
		{"Intel(R) Client Systems", "NUC11PAHi5", "nuc"},
		{"ASUSTeK COMPUTER INC.", "NUC14RVHi7", "nuc"},
		{"ASUSTeK COMPUTER INC.", "ASUS Zenbook 14 UX3405MA_UX3405MA", "asus"},
		{"SAMSUNG ELECTRONICS CO., LTD.", "960XGK", "samsung"},
		{"QEMU", "Standard PC (Q35 + ICH9, 2009)", ""},
	} {
		h := &Hardware{Vendor: c.vendor, Model: c.model}
		if got := h.KnownVendor(); got != c.want {
			t.Errorf("%s / %s: %q, want %q", c.vendor, c.model, got, c.want)
		}
	}
}
