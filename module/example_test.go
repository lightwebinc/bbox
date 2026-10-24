package module_test

import (
	"fmt"
	"os"
	"sort"

	bmodule "github.com/lightwebinc/bcommon/module"

	"github.com/lightwebinc/bbox/module"
)

// quiet is a host that keeps no log or metrics.
type quiet struct{}

func (quiet) Log(string, map[string]any)             {}
func (quiet) Metrics() bmodule.Metrics               { return quiet{} }
func (quiet) Inc(string, map[string]string, float64) {}
func (quiet) Preset(string, map[string]string)       {}
func (quiet) Gauge(string, func() float64)           {}

// A host builds the module from its environment: one topic manager per
// office and ls_bbox for all of them.
func ExampleNew() {
	dir, err := os.MkdirTemp("", "bbox-module-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	env := map[string]string{
		"BBOX_OFFICES":   "example_office_qzxkvbmwtr",
		"BBOX_STATE_DIR": dir,
		"OVERLAY_TOPICS": "tm_bbox_example_office_qzxkvbmwtr",
	}
	m, err := module.New(quiet{}, func(k string) string { return env[k] })
	if err != nil {
		panic(err)
	}
	defer m.Close()
	var names []string
	for t := range m.Module.Topics {
		names = append(names, t)
	}
	for l := range m.Module.Lookups {
		names = append(names, l)
	}
	sort.Strings(names)
	fmt.Println(names)
	// Output: [ls_bbox tm_bbox_example_office_qzxkvbmwtr]
}
