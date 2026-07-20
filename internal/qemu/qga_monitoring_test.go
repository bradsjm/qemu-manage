package qemu

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bradsjm/qemu-manage/internal/backend"
)

func TestDecodeGuestInfoCapabilities(t *testing.T) {
	info, err := decodeGuestInfo(json.RawMessage(`{"version":"9.1","supported_commands":[{"name":"guest-ping","enabled":true,"success-response":true},{"name":"guest-get-load","enabled":false}],"unknown":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "9.1" || !info.Capabilities["guest-ping"] || info.Capabilities["guest-get-load"] {
		t.Fatalf("info = %#v", info)
	}
}

func TestDecodeGuestNetworksCanonicalAddresses(t *testing.T) {
	raw := json.RawMessage(`[
		{"name":"eth1","ip-addresses":[],"unknown":true},
		{"name":"eth0","ip-addresses":[
			{"ip-address":"2001:0db8:0:0::1","ip-address-type":"ipv6","prefix":64},
			{"ip-address":"192.0.2.1","ip-address-type":"ipv4","prefix":24},
			{"ip-address":"192.0.2.1","ip-address-type":"ipv4","prefix":24}
		]},
		{"name":"ignored"}
	]`)
	interfaces, err := decodeGuestNetworks(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(interfaces) != 2 || interfaces[0].Name != "eth0" || interfaces[1].Name != "eth1" {
		t.Fatalf("interfaces = %#v", interfaces)
	}
	if !interfaces[0].AddressesPresent || len(interfaces[0].Addresses) != 2 ||
		interfaces[0].Addresses[0] != (backend.GuestIPAddress{Address: "192.0.2.1", Family: "ipv4", Prefix: 24}) ||
		interfaces[0].Addresses[1] != (backend.GuestIPAddress{Address: "2001:db8::1", Family: "ipv6", Prefix: 64}) {
		t.Fatalf("addresses = %#v", interfaces[0].Addresses)
	}
	if !interfaces[1].AddressesPresent || interfaces[1].Addresses == nil || len(interfaces[1].Addresses) != 0 {
		t.Fatalf("present-empty addresses = %#v", interfaces[1])
	}
}

func TestDecodeGuestNetworksRejectsMalformedAddresses(t *testing.T) {
	for name, raw := range map[string]string{
		"address": `[{"name":"eth0","ip-addresses":[{"ip-address":"bad","ip-address-type":"ipv4","prefix":24}]}]`,
		"family":  `[{"name":"eth0","ip-addresses":[{"ip-address":"192.0.2.1","ip-address-type":"ipv6","prefix":24}]}]`,
		"prefix":  `[{"name":"eth0","ip-addresses":[{"ip-address":"2001:db8::1","ip-address-type":"ipv6","prefix":129}]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeGuestNetworks(json.RawMessage(raw)); err == nil {
				t.Fatal("malformed address accepted")
			}
		})
	}
}

func TestDecodeGuestMetricFamilies(t *testing.T) {
	cpus, err := decodeGuestCPU(json.RawMessage(`[{"cpu":1,"user":0,"system":1500,"unknown":1},{"cpu":0,"idle":2000}]`))
	if err != nil || len(cpus) != 2 || cpus[0].CPU != 0 || cpus[1].Seconds["system"] != 1.5 || cpus[1].Seconds["user"] != 0 {
		t.Fatalf("CPU = %#v, %v", cpus, err)
	}
	if _, err := decodeGuestCPU(json.RawMessage(`[{"cpu":0,"user":-1}]`)); err == nil || !strings.Contains(err.Error(), "nonnegative") {
		t.Fatalf("negative CPU error = %v", err)
	}

	disks, err := decodeGuestDisks(json.RawMessage(`[{"name":"vda","stats":{"read-sectors":0,"write-ios":2,"read-ticks":1500,"ios-progress":0},"unknown":true}]`))
	if err != nil || len(disks) != 1 || disks[0].ReadSectors == nil || *disks[0].ReadSectors != 0 || disks[0].ReadSeconds == nil || *disks[0].ReadSeconds != 1.5 {
		t.Fatalf("disks = %#v, %v", disks, err)
	}

	observation := backend.GuestObservation{}
	midpoint := time.Unix(100, 0)
	if err := decodeGuestFamily("clock", json.RawMessage(`101000000000`), midpoint, &observation); err != nil || observation.ClockOffset == nil || *observation.ClockOffset != 1 {
		t.Fatalf("clock offset = %#v, %v", observation.ClockOffset, err)
	}
}
