// Copyright 2017 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This is a sample chained plugin that supports multiple CNI versions. It
// parses prevResult according to the cniVersion
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

type PluginConf struct {
	types.NetConf

	RuntimeConfig *struct {
		PodAnnotations map[string]string `json:"io.kubernetes.cri.pod-annotations"`
	} `json:"runtimeConfig"`

	DaemonPort           int32 `json:"daemonPort"`
	MaxWaitTimeInSeconds int32 `json:"maxWaitTimeInSeconds"`
}

// parseConfig parses the supplied configuration (and prevResult) from stdin.
func parseConfig(stdin []byte) (*PluginConf, error) {
	conf := PluginConf{}

	if err := json.Unmarshal(stdin, &conf); err != nil {
		return nil, fmt.Errorf("failed to parse network configuration: %v", err)
	}

	// Parse previous result. This will parse, validate, and place the
	// previous result object into conf.PrevResult. If you need to modify
	// or inspect the PrevResult you will need to convert it to a concrete
	// versioned Result struct.
	if err := version.ParsePrevResult(&conf.NetConf); err != nil {
		return nil, fmt.Errorf("could not parse prevResult: %v", err)
	}

	//check if port is set
	if conf.DaemonPort == 0 {
		return nil, fmt.Errorf("daemonPort must be set")
	}

	return &conf, nil
}

// cmdAdd is called for ADD requests
func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	result, err := callChain(conf)
	if err != nil {
		return err
	}

	slotName, err := extractPodNameWithNamespaceFromCniArgs(args)
	if err != nil {
		return err
	}

	err = Wait(slotName, conf)
	if err != nil {
		return err
	}

	return types.PrintResult(result, conf.CNIVersion)
}

func extractPodNameWithNamespaceFromCniArgs(args *skel.CmdArgs) (string, error) {
	podName := ""
	podNamespace := ""
	for _, arg := range strings.Split(args.Args, ";") {
		if strings.HasPrefix(arg, "K8S_POD_NAME=") {
			return strings.TrimPrefix(arg, "K8S_POD_NAME="), nil
		}

		if strings.HasPrefix(arg, "K8S_POD_NAMESPACE=") {
			podNamespace = strings.TrimPrefix(arg, "K8S_POD_NAMESPACE=")
		}
	}

	if podNamespace != "" && podName != "" {
		return fmt.Sprintf("%s/%s", podNamespace, podName), nil
	}

	return "", fmt.Errorf("K8S_POD_NAME not found in CNI_ARGS")
}

func cmdDel(args *skel.CmdArgs) error {
	// we don't need to do anything here
	return nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("pod-startup-limiter"))
}

func cmdCheck(_ *skel.CmdArgs) error {
	return fmt.Errorf("not implemented")
}

func callChain(conf *PluginConf) (*current.Result, error) {
	if conf.PrevResult == nil {
		return nil, fmt.Errorf("must be called as chained plugin")
	}

	prevResult, err := current.GetResult(conf.PrevResult)
	if err != nil {
		return nil, fmt.Errorf("failed to convert prevResult: %v", err)
	}
	return prevResult, nil
}
