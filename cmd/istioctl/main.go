// Copyright 2017 Istio Authors
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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	"github.com/spf13/cobra"
	"k8s.io/client-go/pkg/api"

	"istio.io/manager/cmd"
	"istio.io/manager/model"

	"k8s.io/client-go/pkg/util/yaml"
)

// Each entry in the multi-doc YAML file used by `istioctl create -f` MUST have this format
type inputDoc struct {
	// Type SHOULD be one of the kinds in model.IstioConfig; a route-rule, ingress-rule, or destination-policy
	Type string      `json:"type,omitempty"`
	Name string      `json:"name,omitempty"`
	Spec interface{} `json:"spec,omitempty"`
	// ParsedSpec will be one of the messages in model.IstioConfig: for example an
	// istio.proxy.v1alpha.config.RouteRule or DestinationPolicy
	ParsedSpec proto.Message `json:"-"`
}

var (
	// input file name
	file string

	key    model.Key
	schema model.ProtoSchema

	postCmd = &cobra.Command{
		Use:   "create",
		Short: "Create policies and rules",
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("create takes no arguments")
			}
			varr, err := readInputs()
			if err != nil {
				return err
			}
			if len(varr) == 0 {
				return errors.New("nothing to create")
			}
			for _, v := range varr {
				if err = setup(v.Type, v.Name); err != nil {
					return err
				}
				err = cmd.Client.Post(key, v.ParsedSpec)
				if err != nil {
					return err
				}
				fmt.Printf("Posted %v %v\n", v.Type, v.Name)
			}

			return nil
		},
	}

	putCmd = &cobra.Command{
		Use:   "replace",
		Short: "Replace policies and rules",
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("replace takes no arguments")
			}
			varr, err := readInputs()
			if err != nil {
				return err
			}
			if len(varr) == 0 {
				return errors.New("nothing to replace")
			}
			for _, v := range varr {
				if err = setup(v.Type, v.Name); err != nil {
					return err
				}
				err = cmd.Client.Put(key, v.ParsedSpec)
				if err != nil {
					return err
				}
				fmt.Printf("Put %v %v\n", v.Type, v.Name)
			}

			return nil
		},
	}

	getCmd = &cobra.Command{
		Use:   "get <type> <name>",
		Short: "Retrieve a policy or rule",
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 2 {
				return fmt.Errorf("provide configuration type and name")
			}
			if err := setup(args[0], args[1]); err != nil {
				return err
			}
			item, exists := cmd.Client.Get(key)
			if !exists {
				return fmt.Errorf("does not exist")
			}
			out, err := schema.ToYAML(item)
			if err != nil {
				return err
			}
			fmt.Print(out)
			return nil
		},
	}

	deleteCmd = &cobra.Command{
		Use:   "delete <type> <name> [<name2> ... <nameN>]",
		Short: "Delete policies or rules",
		RunE: func(c *cobra.Command, args []string) error {
			// If we did not receive a file option, get names of resources to delete from command line
			if file == "" {
				if len(args) < 2 {
					return fmt.Errorf("provide configuration type and name or -f option")
				}
				for i := 1; i < len(args); i++ {
					if err := setup(args[0], args[i]); err != nil {
						return err
					}
					if err := cmd.Client.Delete(key); err != nil {
						return err
					}
					fmt.Printf("Deleted %v %v\n", args[0], args[i])
				}
				return nil
			}

			// As we did get a file option, make sure the command line did not include any resources to delete
			if len(args) != 0 {
				return fmt.Errorf("delete takes no arguments when the file option is used")
			}
			varr, err := readInputs()
			if err != nil {
				return err
			}
			if len(varr) == 0 {
				return errors.New("nothing to delete")
			}
			for _, v := range varr {
				if err = setup(v.Type, v.Name); err != nil {
					return err
				}
				err = cmd.Client.Delete(key)
				if err != nil {
					return err
				}
				fmt.Printf("Deleted %v %v\n", v.Type, v.Name)
			}

			return nil
		},
	}

	listCmd = &cobra.Command{
		Use:   "list <type>",
		Short: "List policies and rules",
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("please specify configuration type (one of %v)", model.IstioConfig.Kinds())
			}
			if err := setup(args[0], ""); err != nil {
				return err
			}

			list, err := cmd.Client.List(key.Kind, key.Namespace)
			if err != nil {
				return fmt.Errorf("error listing %s: %v", key.Kind, err)
			}

			for key, item := range list {
				out, err := schema.ToYAML(item)
				if err != nil {
					fmt.Println(err)
				} else {
					fmt.Printf("kind: %s\n", key.Kind)
					fmt.Printf("name: %s\n", key.Name)
					fmt.Printf("namespace: %s\n", key.Namespace)
					fmt.Println("spec:")
					lines := strings.Split(out, "\n")
					for _, line := range lines {
						if line != "" {
							fmt.Printf("  %s\n", line)
						}
					}
				}
				fmt.Println("---")
			}
			return nil
		},
	}
)

func init() {
	postCmd.PersistentFlags().StringVarP(&file, "file", "f", "",
		"Input file with the content of the configuration objects (if not set, command reads from the standard input)")
	putCmd.PersistentFlags().AddFlag(postCmd.PersistentFlags().Lookup("file"))
	deleteCmd.PersistentFlags().AddFlag(postCmd.PersistentFlags().Lookup("file"))

	cmd.RootCmd.Use = "istioctl"
	cmd.RootCmd.Long = fmt.Sprintf("Istio configuration command line utility. Available configuration types: %v",
		model.IstioConfig.Kinds())
	cmd.RootCmd.AddCommand(postCmd)
	cmd.RootCmd.AddCommand(putCmd)
	cmd.RootCmd.AddCommand(getCmd)
	cmd.RootCmd.AddCommand(listCmd)
	cmd.RootCmd.AddCommand(deleteCmd)
}

func main() {
	if err := cmd.RootCmd.Execute(); err != nil {
		glog.Error(err)
		os.Exit(-1)
	}
}

func setup(kind, name string) error {
	var ok bool
	// set proto schema
	schema, ok = model.IstioConfig[kind]
	if !ok {
		return fmt.Errorf("unknown configuration type %s; use one of %v", kind, model.IstioConfig.Kinds())
	}

	// use default namespace by default
	if cmd.RootFlags.Namespace == "" {
		cmd.RootFlags.Namespace = api.NamespaceDefault
	}

	// set the config key
	key = model.Key{
		Kind:      kind,
		Name:      name,
		Namespace: cmd.RootFlags.Namespace,
	}

	return nil
}

// readInputs reads multiple documents from the input and checks with the schema
func readInputs() ([]inputDoc, error) {

	var reader io.Reader
	var err error

	if file == "" {
		reader = os.Stdin
	} else {
		reader, err = os.Open(file)
		if err != nil {
			return nil, err
		}
	}

	var varr []inputDoc

	// We store route-rules as a YaML stream; there may be more than one decoder.
	yamlDecoder := yaml.NewYAMLOrJSONDecoder(reader, 512*1024)
	for {
		v := inputDoc{}
		err = yamlDecoder.Decode(&v)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cannot parse proto message: %v", err)
		}

		// Do a second decode pass, to get the data into structured format
		byteRule, err := json.Marshal(v.Spec)
		if err != nil {
			return nil, fmt.Errorf("could not encode Spec: %v", err)
		}

		schema, ok := model.IstioConfig[v.Type]
		if !ok {
			return nil, fmt.Errorf("unknown spec type %s", v.Type)
		}
		rr, err := schema.FromJSON(string(byteRule))
		if err != nil {
			return nil, fmt.Errorf("cannot parse proto message: %v", err)
		}
		glog.V(2).Info(fmt.Sprintf("Parsed %v %v into %v %v", v.Type, v.Name, schema.MessageName, rr))

		v.ParsedSpec = rr

		varr = append(varr, v)
	}

	return varr, nil
}
