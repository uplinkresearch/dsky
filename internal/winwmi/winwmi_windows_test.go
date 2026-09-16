package winwmi

import (
	"runtime"
	"syscall"
	"testing"
)

// The failure, reproduced: the calling goroutine is locked to a thread whose
// COM is single-threaded, as go-webview2's init leaves the main thread. A
// query from here must still work.
func TestAQueryWorksFromAThreadWithSingleThreadedCOM(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole32 := syscall.NewLazyDLL("ole32.dll")
	const coinitApartmentThreaded = 2
	r, _, _ := ole32.NewProc("CoInitializeEx").Call(0, coinitApartmentThreaded)
	if int32(r) < 0 {
		t.Fatalf("CoInitializeEx: %08x", r)
	}
	defer ole32.NewProc("CoUninitialize").Call()

	var systems []struct{ Manufacturer string }
	if err := Query("SELECT Manufacturer FROM Win32_ComputerSystem", &systems); err != nil {
		t.Fatalf("a query from a single-threaded COM thread failed: %v", err)
	}
	if len(systems) == 0 {
		t.Error("the query returned nothing")
	}
}
