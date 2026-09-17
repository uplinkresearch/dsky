package catalog

import "testing"

// What each maker's computers report to Windows, against the names their
// catalogs use. The catalog names are the live lists as of September 2026.

func TestSurfaceIdentify(t *testing.T) {
	names := []string{
		"Surface Book 2", "Surface Book 3", "Surface Go 3", "Surface Go 4",
		"Surface Laptop 3 (AMD)", "Surface Laptop 3 (Intel)", "Surface Laptop 4 (AMD)", "Surface Laptop 4 (Intel)",
		"Surface Laptop 5", "Surface Laptop 5G for Business 7th Edition (Intel)", "Surface Laptop 6",
		"Surface Laptop for Business 7th Edition (Intel)", "Surface Laptop for Business 8th Edition (Intel)",
		"Surface Laptop Go", "Surface Laptop Go 2", "Surface Laptop Go 3", "Surface Laptop Studio", "Surface Laptop Studio 2",
		"Surface Pro 10", "Surface Pro 10 with 5G for Business", "Surface Pro 5 (LTE)", "Surface Pro 5 (Wi-Fi)",
		"Surface Pro 6", "Surface Pro 7", "Surface Pro 7+ and Surface Pro 7+ (LTE)", "Surface Pro 8", "Surface Pro 9 (Intel)",
		"Surface Pro for Business 11th Edition (Intel)", "Surface Pro for Business 12th Edition (Intel)",
		"Surface Studio 2", "Surface Studio 2+",
	}
	for _, c := range []struct{ reported, want string }{
		{"Surface Laptop 4 (AMD)", "Surface Laptop 4 (AMD)"},
		{"Surface Laptop 4 (Intel)", "Surface Laptop 4 (Intel)"},
		{"Surface Pro 9 (Intel)", "Surface Pro 9 (Intel)"},
		{"Surface Pro 7+ (Intel)", "Surface Pro 7+ and Surface Pro 7+ (LTE)"},
		{"Surface Pro 7 (Intel)", "Surface Pro 7"},
		{"Surface Laptop 5 (Intel)", "Surface Laptop 5"},
		{"Surface Laptop 6 for Business (Intel)", "Surface Laptop 6"},
		{"Surface Pro 10 for Business (Intel)", "Surface Pro 10"},
		{"Surface Pro 10 with 5G for Business (Intel)", "Surface Pro 10 with 5G for Business"},
		{"Surface Laptop 7th Edition (Intel)", "Surface Laptop for Business 7th Edition (Intel)"},
		{"Surface Laptop Go 2 (Intel)", "Surface Laptop Go 2"},
		{"Surface Studio 2+ (Intel)", "Surface Studio 2+"},
		{"Surface Studio 2 (Intel)", "Surface Studio 2"},
		{"Surface Laptop 4 (Intel)", "Surface Laptop 4 (Intel)"},
		// Snapdragon models are not in the x64 list, and a name that could
		// be several models is none of them.
		{"Surface Laptop 7th Edition (AMD)", ""},
		{"Surface Pro", ""},
	} {
		if got := surfaceIdentify(c.reported, names); got != c.want {
			t.Errorf("%q: %q, want %q", c.reported, got, c.want)
		}
	}
}

func TestASUSIdentify(t *testing.T) {
	names := []string{"UX3405MA", "ASUS AiO A3 (A3402WV)", "ASUS Chromebook CB14 CB1405CKA", "Vivobook 15 (X1504VA)",
		"Vivobook 15 (X1504VA) Special Edition", "A15", "TUF Gaming A15 (FA507NV)", "ROG Strix G16 (G614JV)"}
	for _, c := range []struct{ reported, want string }{
		{"ASUS Zenbook 14 UX3405MA_UX3405MA", "UX3405MA"},
		{"ASUS Vivobook 15 X1504VA_X1504VA", "Vivobook 15 (X1504VA)"},
		{"ASUS TUF Gaming A15 FA507NV_FA507NV", "TUF Gaming A15 (FA507NV)"},
		{"ROG Strix G614JV_G614JV", "ROG Strix G16 (G614JV)"},
		{"ASUS AiO A3402WVAK_A3402WV", "ASUS AiO A3 (A3402WV)"},
		{"ASUS Zenbook 14 UX3405CA_UX3405CA", ""},
	} {
		if got := asusIdentify(c.reported, names); got != c.want {
			t.Errorf("%q: %q, want %q", c.reported, got, c.want)
		}
	}
}

func TestNUCFamily(t *testing.T) {
	for code, want := range map[string]string{
		"NUC13ANKi7": "NUC13AN", "NUC13ANHi7": "NUC13AN", "NUC13ANBI3": "NUC13AN", "nuc14rvh-b": "NUC14RV",
		"NUC8i5BEH": "NUC8BE", "NUC11PAHi5": "NUC11PA", "Surface Go 3": "", "NUC": "",
	} {
		if got := nucFamily(code); got != want {
			t.Errorf("%q: %q, want %q", code, got, want)
		}
	}
}

func TestSamsungIdentify(t *testing.T) {
	models := groupSamsungModels(map[string][]string{
		"Galaxy Book4 Pro": {"NP940XGK-KG1US", "NP944XGK-KG1US", "NP960XGK-KG1US", "NP964XGK-CG2US"},
		"Galaxy Book2 Pro": {"NP950XED-KA1US", "NP950XEE-XA1US"},
		"Galaxy Book3":     {"NP750XFG-KA1US"},
	})
	for _, c := range []struct{ reported, want string }{
		{"960XGK", "Galaxy Book4 Pro (NP960XGK)"},
		{"NP960XGK-KG1US", "Galaxy Book4 Pro (NP960XGK)"},
		{"NP964XGK", "Galaxy Book4 Pro (NP964XGK)"},
		{"950XEE", "Galaxy Book2 Pro (NP950XEE)"},
		{"750XFG", "Galaxy Book3 (NP750XFG)"},
		{"750XFH", ""},
		{"Galaxy Book", ""},
	} {
		if got := samsungIdentify(c.reported, models); got != c.want {
			t.Errorf("%q: %q, want %q", c.reported, got, c.want)
		}
	}
}
