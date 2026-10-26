// Package failpoint lets tests crash the program at a named point to exercise
// journal recovery. Hit is a no-op in release builds (no build tag); with
// `-tags failpoint` it consults SM_FAILPOINTS.
//
// SM_FAILPOINTS="name=action[,name=action...]" where action is one of
//
//	crash        os.Exit(137) immediately, without running defers
//	error        Hit returns errcode.Internal ("failpoint")
//	panic        panic("failpoint " + name)
//
// Names follow "<kind>.after_<state>" and "<kind>.before_<state>" for every
// state in store.Flow (for example "capture.after_stored",
// "install.before_written"), plus "publish.mid_upload" and "install.mid_write"
// for the middle of a multi-step action.
package failpoint

import (
	"os"
	"strings"
)

// Hit is called by production code at each named point.
func Hit(name string) error { return hit(name) }

// parse is shared by the tagged implementation and its tests.
func parse(env string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(env, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok && k != "" {
			out[k] = v
		}
	}
	return out
}

var _ = os.Getenv // used by the tagged implementation
