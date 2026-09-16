// Package winwmi runs WMI queries where COM will let them run.
//
// The WMI library initializes COM on the thread it runs on as multithreaded.
// go-webview2 -- linked into dsky.exe for the app window -- locks the main
// goroutine to the process's main thread in an init function and initializes
// COM there as single-threaded, before main runs. A WMI query made from the
// main goroutine therefore lands on a thread already committed to the other
// threading model, and COM refuses it: "Cannot change thread mode after it is
// set". The portal never met this, because its queries run in request
// goroutines; the command line met it on its first disk listing -- `dsky disks
// list` failed outright on Windows 11.
//
// So every query runs on a goroutine of its own, which is not the one locked to
// the main thread, and the WMI library locks that goroutine to a thread of its
// own choosing.
package winwmi

import "github.com/yusufpapurcu/wmi"

// Query runs a WQL query in the default namespace.
func Query(query string, dst interface{}) error {
	return QueryNamespace(query, dst, "")
}

// QueryNamespace runs a WQL query in the given namespace; "" is the default.
func QueryNamespace(query string, dst interface{}, namespace string) error {
	done := make(chan error, 1)
	go func() {
		if namespace == "" {
			done <- wmi.Query(query, dst)
			return
		}
		done <- wmi.QueryNamespace(query, dst, namespace)
	}()
	return <-done
}
