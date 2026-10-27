// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudru

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/clouds/vm"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// The plugin's VmHosts handler builds a single-host response with the three
// Pulumi resources the in-tree driver builds (tls.PrivateKey, local.Command,
// remote.Command) and surfaces the host's name + user + role + depends-on in
// the shape BuildVmHosts enforces. Address flows through AddressOutput —
// preview-aware, so the mock run (which never actually executes the local
// Command) gets a known empty string.
func TestVmHosts_BuildsOneHost(t *testing.T) {
	RegisterTestingT(t)

	cfg := vm.VmConfig{
		Driver:      vm.DriverCloudruEvolution,
		Image:       "ubuntu-2404-lts",
		SshUser:     "sc",
		Credentials: api.Credentials{Credentials: evolutionCredsJSON(t)},
	}
	cfgJSON, err := json.Marshal(cfg)
	Expect(err).ToNot(HaveOccurred())

	req := &scpluginv1.VmHostsRequest{
		Header: &scpluginv1.RequestHeader{
			StackName:    "test",
			ProjectName:  "proj",
			Environment:  "staging",
			RunToken:     "test-token",
			TraceId:      "test-trace",
			ProviderUrns: map[string]string{},
		},
		Driver:       vm.DriverCloudruEvolution,
		ResourceName: "my-vm",
		ConfigJson:   cfgJSON,
		Ports: []*scpluginv1.VmPortRange{
			{Label: "ssh", Protocol: "tcp", From: 22, To: 22},
			{Label: "https", Protocol: "tcp", From: 443, To: 443},
		},
	}

	var resp *scpluginv1.VmHostsResponse
	err = sdk.RunErr(func(ctx *sdk.Context) error {
		r, err := VmHosts(ctx, req)
		if err != nil {
			return err
		}
		resp = r
		return nil
	}, sdk.WithMocks("proj", "staging", vmHostsMocks{}))
	Expect(err).ToNot(HaveOccurred())

	Expect(resp.GetHosts()).To(HaveLen(1), "one `type: vm` resource = one host")
	h := resp.GetHosts()[0]
	Expect(h.GetName()).To(Equal("my-vm-host-0"), "name derived from the request's resource name, stable across deploys")
	Expect(h.GetUser()).To(Equal("sc"))
	Expect(h.GetPort()).To(Equal(int32(22)))
	Expect(h.GetRole()).To(Equal(string(vm.RoleCompose)))

	// AddressOutput awaited an unknown mock stdout, so the preview-safe empty
	// string lands on the wire. Core-side sees the shape it already handles.
	Expect(h.GetAddress()).ToNot(BeNil())
	Expect(h.GetAddress().GetLiteral()).To(Equal(""))

	// PrivateKeyOf emits a ref on the tls.PrivateKey registered by VmHosts.
	Expect(h.GetPrivateKey()).ToNot(BeNil())
	Expect(h.GetPrivateKey().GetRef()).ToNot(BeNil(), "key is a resource property, not a literal")
	Expect(h.GetPrivateKey().GetRef().GetProperty()).To(Equal("privateKeyOpenssh"))
	Expect(h.GetPrivateKey().GetSecret()).To(BeTrue())

	// TrustOnFirstUse: no host key, pinned=false.
	Expect(h.GetHostKey()).To(BeNil())
	Expect(h.GetHostKeyPinned()).To(BeFalse())

	// DependsOn carries two URNs — vmCmd and dockerCmd. Both must be present
	// so core sequences the compose/k3s steps after docker is installed.
	Expect(h.GetDependsOn()).To(HaveLen(2))
}

// A `network:` block is refused outright rather than silently dropped: the
// cloud's `POST /vms` rejects every spelling of a subnet or security group,
// so an accepted-looking `network:` would describe a placement that silently
// did not happen.
func TestVmHosts_RefusesNetworkBlock(t *testing.T) {
	RegisterTestingT(t)

	cfg := vm.VmConfig{
		Driver:      vm.DriverCloudruEvolution,
		Image:       "ubuntu-2404-lts",
		SshUser:     "sc",
		Credentials: api.Credentials{Credentials: evolutionCredsJSON(t)},
		Network:     &vm.VmNetwork{ID: "net-abc"},
	}
	cfgJSON, err := json.Marshal(cfg)
	Expect(err).ToNot(HaveOccurred())

	req := &scpluginv1.VmHostsRequest{
		Header:       &scpluginv1.RequestHeader{Environment: "staging"},
		Driver:       vm.DriverCloudruEvolution,
		ResourceName: "my-vm",
		ConfigJson:   cfgJSON,
	}

	err = sdk.RunErr(func(ctx *sdk.Context) error {
		_, err := VmHosts(ctx, req)
		return err
	}, sdk.WithMocks("proj", "staging", vmHostsMocks{}))
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring("`network` is not supported"))
	Expect(err.Error()).To(ContainSubstring("extra_forbidden"))
	Expect(err.Error()).To(ContainSubstring(vm.DriverCloudruEvolution))
	Expect(err.Error()).To(ContainSubstring(vm.DriverByo), "the error must point operators at the byo path")
}

// Credentials that do not parse — the shape caught at the earliest layer, so
// an operator sees the explanation at parse time rather than a cloud 401.
func TestVmHosts_RefusesInvalidCredentials(t *testing.T) {
	RegisterTestingT(t)

	cfg := vm.VmConfig{
		Driver:      vm.DriverCloudruEvolution,
		Image:       "ubuntu-2404-lts",
		SshUser:     "sc",
		Credentials: api.Credentials{Credentials: `{"malformed": `}, // not JSON
	}
	cfgJSON, err := json.Marshal(cfg)
	Expect(err).ToNot(HaveOccurred())

	req := &scpluginv1.VmHostsRequest{
		Header:       &scpluginv1.RequestHeader{Environment: "staging"},
		Driver:       vm.DriverCloudruEvolution,
		ResourceName: "my-vm",
		ConfigJson:   cfgJSON,
	}

	err = sdk.RunErr(func(ctx *sdk.Context) error {
		_, err := VmHosts(ctx, req)
		return err
	}, sdk.WithMocks("proj", "staging", vmHostsMocks{}))
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring("Evolution credentials"))
}

// evolutionCredsJSON returns a syntactically valid EvolutionAccountConfig
// credentials blob. The values are placeholders — the test does not call the
// Cloud.ru API.
func evolutionCredsJSON(t *testing.T) string {
	t.Helper()
	creds := map[string]string{
		"keyId":     "test-key-id",
		"keySecret": "test-key-secret",
		"projectId": "test-project-id",
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("encode creds: %v", err)
	}
	return string(raw)
}

// vmHostsMocks answers resource creates for the three resources VmHosts
// registers: tls.PrivateKey (algorithm = ED25519), local.Command (apply +
// destroy scripts), and remote.Command (docker bootstrap).
type vmHostsMocks struct{}

func (vmHostsMocks) NewResource(args sdk.MockResourceArgs) (string, resource.PropertyMap, error) {
	switch args.TypeToken {
	case "tls:index/privateKey:PrivateKey":
		return args.Name + "-id", resource.PropertyMap{
			"privateKeyOpenssh": resource.NewStringProperty("-----BEGIN OPENSSH PRIVATE KEY-----"),
			"publicKeyOpenssh":  resource.NewStringProperty("ssh-ed25519 AAAAC3Nz test"),
		}, nil
	case "command:local:Command", "command:remote:Command":
		return args.Name + "-id", resource.PropertyMap{
			"stdout": resource.NewStringProperty(""),
		}, nil
	}
	return args.Name + "-id", args.Inputs, nil
}

func (vmHostsMocks) Call(args sdk.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}
