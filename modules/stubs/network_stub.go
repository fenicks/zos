package stubs

import (
	zbus "github.com/threefoldtech/zbus"
	modules "github.com/threefoldtech/zosv2/modules"
)

type NetworkerStub struct {
	client zbus.Client
	module string
	object zbus.ObjectID
}

func NewNetworkerStub(client zbus.Client) *NetworkerStub {
	return &NetworkerStub{
		client: client,
		module: "network",
		object: zbus.ObjectID{
			Name:    "network",
			Version: "0.0.1",
		},
	}
}

func (s *NetworkerStub) ApplyNetResource(arg0 modules.NetResource) (ret0 error) {
	args := []interface{}{arg0}
	result, err := s.client.Request(s.module, s.object, "ApplyNetResource", args...)
	if err != nil {
		panic(err)
	}
	ret0 = new(zbus.RemoteError)
	if err := result.Unmarshal(0, &ret0); err != nil {
		panic(err)
	}
	return
}

func (s *NetworkerStub) GetNetResource(arg0 string) (ret0 modules.NetResource, ret1 error) {
	args := []interface{}{arg0}
	result, err := s.client.Request(s.module, s.object, "GetNetResource", args...)
	if err != nil {
		panic(err)
	}
	if err := result.Unmarshal(0, &ret0); err != nil {
		panic(err)
	}
	ret1 = new(zbus.RemoteError)
	if err := result.Unmarshal(1, &ret1); err != nil {
		panic(err)
	}
	return
}