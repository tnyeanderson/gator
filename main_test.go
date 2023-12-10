package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	jsonpatch "github.com/evanphx/json-patch"
)

func ExamplePluginNoOp() {
	stdin := []byte(`{"type": "gator", "plugin": "debug", "prevResult": {"key": "value"}}`)
	conf, _ := parseConf(stdin)
	out, _ := formatTestJSON(conf.downstreamConfig)
	fmt.Println(string(out))

	// Output:
	// {
	//   "prevResult": {
	//     "key": "value"
	//   },
	//   "type": "debug"
	// }
}

func ExamplePluginRouteOverride() {
	stdin, _ := mergePrevResult("testdata/route-override.json")
	conf, _ := parseConf(stdin)
	out, _ := formatTestJSON(conf.downstreamConfig)
	fmt.Println(string(out))

	// Output:
	// {
	//   "addroutes": [
	//     {
	//       "dst": "10.96.0.0/16",
	//       "gw": "10.244.1.1"
	//     }
	//   ],
	//   "prevResult": {
	//     "cniVersion": "0.3.1",
	//     "dns": {},
	//     "interfaces": [
	//       {
	//         "mac": "00:00:00:00:00:01",
	//         "name": "cni0"
	//       },
	//       {
	//         "mac": "00:00:00:00:00:02",
	//         "name": "veth99999999"
	//       },
	//       {
	//         "mac": "00:00:00:00:00:03",
	//         "name": "eth0",
	//         "sandbox": "/var/run/netns/cni-00000000-1111-2222-3333-444444444444"
	//       }
	//     ],
	//     "ips": [
	//       {
	//         "address": "10.244.1.42/24",
	//         "gateway": "10.244.1.1",
	//         "interface": 2,
	//         "version": "4"
	//       }
	//     ],
	//     "routes": [
	//       {
	//         "dst": "10.244.0.0/16"
	//       },
	//       {
	//         "dst": "0.0.0.0/0",
	//         "gw": "10.244.1.1"
	//       }
	//     ]
	//   },
	//   "type": "route-override"
	// }

}

func ExamplePluginDebug() {
	// This debug.json file's patch is time-based. This test will have to be
	// updated each year.
	stdin, _ := mergePrevResult("testdata/debug.json")
	conf, _ := parseConf(stdin)
	out, _ := formatTestJSON(conf.downstreamConfig)
	fmt.Println(string(out))

	// Output:
	// {
	//   "addHooks": [
	//     [
	//       "sh",
	//       "-c",
	//       "ip link set $CNI_IFNAME promisc on"
	//     ]
	//   ],
	//   "cniOutput": "/tmp/cni-output-2023.log",
	//   "prevResult": {
	//     "cniVersion": "0.3.1",
	//     "dns": {},
	//     "interfaces": [
	//       {
	//         "mac": "00:00:00:00:00:01",
	//         "name": "cni0"
	//       },
	//       {
	//         "mac": "00:00:00:00:00:02",
	//         "name": "veth99999999"
	//       },
	//       {
	//         "mac": "00:00:00:00:00:03",
	//         "name": "eth0",
	//         "sandbox": "/var/run/netns/cni-00000000-1111-2222-3333-444444444444"
	//       }
	//     ],
	//     "ips": [
	//       {
	//         "address": "10.244.1.42/24",
	//         "gateway": "10.244.1.1",
	//         "interface": 2,
	//         "version": "4"
	//       }
	//     ],
	//     "routes": [
	//       {
	//         "dst": "10.244.0.0/16"
	//       },
	//       {
	//         "dst": "0.0.0.0/0",
	//         "gw": "10.244.1.1"
	//       }
	//     ]
	//   },
	//   "type": "debug"
	// }

}

func mergePrevResult(file string) ([]byte, error) {
	conf, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	prevResult, err := os.ReadFile("testdata/prevresult.json")
	if err != nil {
		return nil, err
	}

	return jsonpatch.MergePatch(conf, prevResult)
}

func formatTestJSON(j []byte) ([]byte, error) {
	b := &bytes.Buffer{}
	if err := json.Indent(b, j, "", "  "); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
