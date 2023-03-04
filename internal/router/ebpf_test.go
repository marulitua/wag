package router

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/NHAS/wag/internal/config"
	"github.com/NHAS/wag/internal/data"
	"github.com/NHAS/wag/internal/routetypes"

	"github.com/cilium/ebpf"
	"golang.org/x/net/ipv4"
)

func TestBasicLoad(t *testing.T) {
	if err := setup("../config/test_in_memory_db.json"); err != nil {
		t.Fatal(err)
	}
	defer xdpObjects.Close()
}

func TestBlankPacket(t *testing.T) {

	if err := setup("../config/test_in_memory_db.json"); err != nil {
		t.Fatal(err)
	}
	defer xdpObjects.Close()

	buff := make([]byte, 15)
	value, _, err := xdpObjects.bpfPrograms.XdpWagFirewall.Test(buff)
	if err != nil {
		t.Fatalf("program failed %s", err)
	}

	if result(value) != "XDP_DROP" {
		t.Fatal("program did not drop a completely blank packet: did", result(value))
	}
}

func TestAddNewDevices(t *testing.T) {

	if err := setup("../config/test_in_memory_db.json"); err != nil {
		t.Fatal(err)
	}
	defer xdpObjects.Close()

	out, err := addDevices()
	if err != nil {
		t.Fatal(err)
	}

	var ipBytes []byte
	var deviceBytes = make([]byte, 40)

	found := map[string]bool{}

	iter := xdpObjects.Devices.Iterate()
	for iter.Next(&ipBytes, &deviceBytes) {
		ip := net.IP(ipBytes)

		var newDevice fwentry
		err := newDevice.Unpack(deviceBytes)
		if err != nil {
			t.Fatal("unpacking new device:", err)
		}

		if newDevice.lastPacketTime != 0 || newDevice.sessionExpiry != 0 {
			t.Fatal("timers were not 0 immediately after device add")
		}
		found[ip.String()] = true
	}

	if iter.Err() != nil {
		t.Fatalf("iterator reported an error: %s", iter.Err())
	}

	if len(found) != len(out) {
		t.Fatalf("expected number of devices not found when iterating timestamp map %d != %d", len(found), len(out))
	}

	for _, device := range out {
		if !found[device.Address] {
			t.Fatalf("%s not found even though it should have been added", device.Address)
		}
	}

}

func TestAddUser(t *testing.T) {

	if err := setup("../config/test_in_memory_db.json"); err != nil {
		t.Fatal(err)
	}
	defer xdpObjects.Close()

	out, err := addDevices()
	if err != nil {
		t.Fatal(err)
	}

	for _, device := range out {
		publicAcls, err := checkLPMMap(device.Username, xdpObjects.PublicTable)
		if err != nil {
			t.Fatal("checking publictable:", err)
		}

		mfaAcls, err := checkLPMMap(device.Username, xdpObjects.MfaTable)
		if err != nil {
			t.Fatal("checking mfatable:", err)
		}

		acl := config.GetEffectiveAcl(device.Username)

		results, err := routetypes.ParseRules(acl.Allow)
		if err != nil {
			t.Fatal("parsing rules failed?:", err)
		}

		var allow []string
		for _, r := range results {

			for _, k := range r.Keys {
				allow = append(allow, k.String())
			}
		}

		results, err = routetypes.ParseRules(acl.Mfa)
		if err != nil {
			t.Fatal("parsing rules failed?:", err)
		}

		var mfa []string
		for _, r := range results {

			for _, k := range r.Keys {
				mfa = append(mfa, k.String())
			}
		}

		if !sameStringSlice(allow, publicAcls) {
			t.Fatal("public allow list does not match configured acls\n got: ", publicAcls, "\nexpected:", allow)
		}

		if !sameStringSlice(mfa, mfaAcls) {
			t.Fatal("mfa allow list does not match configured acls\n got: ", mfaAcls, "\nexpected:", mfa)
		}

	}
}

func TestRoutePriority(t *testing.T) {

	if err := setup("../config/test_roaming_all_routes_mfa_priority.json"); err != nil {
		t.Fatal(err)
	}
	defer xdpObjects.Close()

	out, err := addDevices()
	if err != nil {
		t.Fatal(err)
	}

	headers := []ipv4.Header{

		{
			Version: 4,
			Dst:     net.ParseIP("8.8.8.8"),
			Src:     net.ParseIP(out[0].Address),
			Len:     ipv4.HeaderLen,
		},
		{
			Version: 4,
			Dst:     net.ParseIP("11.11.11.11"),
			Src:     net.ParseIP(out[0].Address),
			Len:     ipv4.HeaderLen,
		},
		{
			Version: 4,
			Dst:     net.ParseIP("1.1.1.1"),
			Src:     net.ParseIP(out[0].Address),
			Len:     ipv4.HeaderLen,
		},
		{
			Version: 4,
			Dst:     net.ParseIP(out[0].Address),
			Src:     net.ParseIP("1.1.1.1"),
			Len:     ipv4.HeaderLen,
		}, {
			Version: 4,
			Dst:     net.ParseIP("192.168.1.1"),
			Src:     net.ParseIP(out[0].Address),
			Len:     ipv4.HeaderLen,
		},
	}

	expectedResults := map[string]uint32{
		headers[0].String(): XDP_DROP,
		headers[1].String(): XDP_PASS,
		headers[2].String(): XDP_PASS,
		headers[3].String(): XDP_PASS,
		headers[4].String(): XDP_PASS,
	}

	for i := range headers {
		if headers[i].Src == nil || headers[i].Dst == nil {
			t.Fatal("could not parse ip")
		}

		packet, err := headers[i].Marshal()
		if err != nil {
			t.Fatal(err)
		}

		value, _, err := xdpObjects.bpfPrograms.XdpWagFirewall.Test(packet)
		if err != nil {
			t.Fatalf("program failed %s", err)
		}

		if result(value) != result(expectedResults[headers[i].String()]) {
			t.Logf("(%s) program did not %s packet instead did: %s", headers[i].String(), result(expectedResults[headers[i].String()]), result(value))
			t.Fail()
		}
	}

}

func TestBasicAuthorise(t *testing.T) {
	if err := setup("../config/test_in_memory_db.json"); err != nil {
		t.Fatal(err)
	}
	defer xdpObjects.Close()

	out, err := addDevices()
	if err != nil {
		t.Fatal(err)
	}

	err = SetAuthorized(out[0].Address, out[0].Username)
	if err != nil {
		t.Fatal(err)
	}

	if !IsAuthed(out[0].Address) {
		t.Fatal("after setting user as authorized it should be.... authorized")
	}

	headers := []ipv4.Header{
		{
			Version: 4,
			Dst:     net.ParseIP("11.11.11.11"),
			Src:     net.ParseIP(out[0].Address),
			Len:     ipv4.HeaderLen,
		},
		{
			Version: 4,
			Dst:     net.ParseIP("192.168.3.11"),
			Src:     net.ParseIP(out[0].Address),
			Len:     ipv4.HeaderLen,
		},
		{
			Version: 4,
			Dst:     net.ParseIP("8.8.8.8"),
			Src:     net.ParseIP(out[0].Address),
			Len:     ipv4.HeaderLen,
		},
		{
			Version: 4,
			Dst:     net.ParseIP("3.21.11.11"),
			Src:     net.ParseIP(out[1].Address),
			Len:     ipv4.HeaderLen,
		},
		{
			Version: 4,
			Dst:     net.ParseIP("7.7.7.7"),
			Src:     net.ParseIP(out[1].Address),
			Len:     ipv4.HeaderLen,
		},
		{
			Version: 4,
			Dst:     net.ParseIP("4.3.3.3"),
			Src:     net.ParseIP(out[1].Address),
			Len:     ipv4.HeaderLen,
		},
	}

	expectedResults := map[string]uint32{
		headers[0].String(): XDP_DROP,
		headers[1].String(): XDP_PASS,
		headers[2].String(): XDP_PASS,
		headers[3].String(): XDP_DROP,
		headers[4].String(): XDP_PASS,
		headers[5].String(): XDP_DROP,
	}

	mfas := config.GetEffectiveAcl(out[0].Username).Mfa
	for i := range mfas {

		rule, err := routetypes.ParseRule(mfas[i])
		if err != nil {
			t.Fatal("could not parse ip: ", err)
		}

		if len(rule.Keys) != 1 {
			t.Fatal("expected to only have one key")
		}

		newHeader := ipv4.Header{
			Version: 4,
			Dst:     rule.Keys[0].AsIP(),
			Src:     net.ParseIP(out[0].Address),
			Len:     ipv4.HeaderLen,
		}
		headers = append(headers, newHeader)

		expectedResults[newHeader.String()] = XDP_PASS

	}

	for i := range headers {
		if headers[i].Src == nil || headers[i].Dst == nil {
			t.Fatal("could not parse ip")
		}

		packet, err := headers[i].Marshal()
		if err != nil {
			t.Fatal(err)
		}

		value, _, err := xdpObjects.bpfPrograms.XdpWagFirewall.Test(packet)
		if err != nil {
			t.Fatalf("program failed %s", err)
		}

		if value != expectedResults[headers[i].String()] {
			m, _ := GetRules()

			r, _ := json.MarshalIndent(m, "", "    ")
			log.Printf("%s\n", string(r))
			t.Fatalf("%s program did not %s packet instead did: %s", headers[i].String(), result(expectedResults[headers[i].String()]), result(value))
		}
	}

	err = Deauthenticate(out[0].Address)
	if err != nil {
		t.Fatal(err)
	}

	if IsAuthed(out[0].Address) {
		t.Fatal("after setting user as deauthorized it should be.... deauthorized")
	}

	for i := range headers {
		if headers[i].Src == nil || headers[i].Dst == nil {
			t.Fatal("could not parse ip")
		}

		if out[0].Address != headers[i].Src.String() {
			continue
		}

		packet, err := headers[i].Marshal()
		if err != nil {
			t.Fatal(err)
		}

		value, _, err := xdpObjects.bpfPrograms.XdpWagFirewall.Test(packet)
		if err != nil {
			t.Fatalf("program failed %s", err)
		}

		if value != XDP_DROP {
			t.Fatalf("after deauthenticating, everything should be XDP_DROP instead %s", result(value))
		}
	}

}

func TestSlidingWindow(t *testing.T) {
	if err := setup("../config/test_disabled_max_lifetime.json"); err != nil {
		t.Fatal(err)
	}
	defer xdpObjects.Close()

	out, err := addDevices()
	if err != nil {
		t.Fatal(err)
	}

	err = SetAuthorized(out[0].Address, out[0].Username)
	if err != nil {
		t.Fatal(err)
	}

	if !IsAuthed(out[0].Address) {
		t.Fatal("after setting user as authorized it should be.... authorized")
	}

	ip, _, err := net.ParseCIDR(config.GetEffectiveAcl(out[0].Username).Mfa[0])
	if err != nil {
		t.Fatal("could not parse ip: ", err)
	}

	testAuthorizedPacket := ipv4.Header{
		Version: 4,
		Dst:     ip,
		Src:     net.ParseIP(out[0].Address),
		Len:     ipv4.HeaderLen,
	}

	log.Println(testAuthorizedPacket.Dst, testAuthorizedPacket.Src)

	if testAuthorizedPacket.Src == nil || testAuthorizedPacket.Dst == nil {
		t.Fatal("could not parse ip")
	}

	packet, err := testAuthorizedPacket.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	var beforeDevice fwentry
	deviceBytes, err := xdpObjects.Devices.LookupBytes(net.ParseIP(out[0].Address).To4())
	if err != nil {
		t.Fatal(err)
	}

	err = beforeDevice.Unpack(deviceBytes)
	if err != nil {
		t.Fatal(err)
	}

	var timeoutFromMap uint64
	err = xdpObjects.InactivityTimeoutMinutes.Lookup(uint32(0), &timeoutFromMap)
	if err != nil {
		t.Fatal(err)
	}

	difference := uint64(config.Values().SessionInactivityTimeoutMinutes) * 60000000000
	if timeoutFromMap != difference {
		t.Fatal("timeout retrieved from ebpf program does not match json")
	}

	value, _, err := xdpObjects.bpfPrograms.XdpWagFirewall.Test(packet)
	if err != nil {
		t.Fatalf("program failed %s", err)
	}

	if value != 2 {
		t.Fatalf("program did not %s packet instead did: %s", result(2), result(value))
	}

	var afterDevice fwentry
	deviceBytes, err = xdpObjects.Devices.LookupBytes(net.ParseIP(out[0].Address).To4())
	if err != nil {
		t.Fatal(err)
	}

	err = afterDevice.Unpack(deviceBytes)
	if err != nil {
		t.Fatal(err)
	}

	if afterDevice.lastPacketTime == beforeDevice.lastPacketTime {
		t.Fatal("sending a packet did not change sliding window timeout")
	}

	if afterDevice.lastPacketTime < beforeDevice.lastPacketTime {
		t.Fatal("the resulting update must be closer in time")
	}

	t.Logf("Now doing timing test for sliding window waiting %d+10seconds", config.Values().SessionInactivityTimeoutMinutes)

	//Check slightly after inactivity timeout to see if the user is now not authenticated
	time.Sleep(time.Duration(config.Values().SessionInactivityTimeoutMinutes)*time.Minute + 10*time.Second)

	value, _, err = xdpObjects.bpfPrograms.XdpWagFirewall.Test(packet)
	if err != nil {
		t.Fatalf("program failed %s", err)
	}

	if value != 1 {
		t.Fatalf("program did not %s packet instead did: %s", result(1), result(value))
	}

	if IsAuthed(out[0].Address) {
		t.Fatal("user is still authorized after inactivity timeout should have killed them")
	}
}

func TestDisabledSlidingWindow(t *testing.T) {
	if err := setup("../config/test_disabled_sliding_window.json"); err != nil {
		t.Fatal(err)
	}
	defer xdpObjects.Close()

	out, err := addDevices()
	if err != nil {
		t.Fatal(err)
	}

	var timeoutFromMap uint64
	err = xdpObjects.InactivityTimeoutMinutes.Lookup(uint32(0), &timeoutFromMap)
	if err != nil {
		t.Fatal(err)
	}

	if timeoutFromMap != math.MaxUint64 {
		t.Fatalf("the inactivity timeout was not set to max uint64, was %d (maxuint64 %d)", timeoutFromMap, uint64(math.MaxUint64))
	}

	err = SetAuthorized(out[0].Address, out[0].Username)
	if err != nil {
		t.Fatal(err)
	}

	if !IsAuthed(out[0].Address) {
		t.Fatal("after setting user as authorized it should be.... authorized")
	}

	ip, _, err := net.ParseCIDR(config.GetEffectiveAcl(out[0].Username).Mfa[0])
	if err != nil {
		t.Fatal("could not parse ip: ", err)
	}

	testAuthorizedPacket := ipv4.Header{
		Version: 4,
		Dst:     ip,
		Src:     net.ParseIP(out[0].Address),
		Len:     ipv4.HeaderLen,
	}

	if testAuthorizedPacket.Src == nil || testAuthorizedPacket.Dst == nil {
		t.Fatal("could not parse ip")
	}

	packet, err := testAuthorizedPacket.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("Now doing timing test for disabled sliding window waiting...")

	elapsed := 0
	for {
		time.Sleep(15 * time.Second)
		elapsed += 15

		value, _, err := xdpObjects.bpfPrograms.XdpWagFirewall.Test(packet)
		if err != nil {
			t.Fatalf("program failed %s", err)
		}

		if value == 1 {
			if elapsed < config.Values().MaxSessionLifetimeMinutes*60 {
				t.Fatal("epbf kernel blocking valid traffic early")
			} else {
				break
			}

		}
	}

}

func TestMaxSessionLifetime(t *testing.T) {
	if err := setup("../config/test_disabled_sliding_window.json"); err != nil {
		t.Fatal(err)
	}
	defer xdpObjects.Close()

	out, err := addDevices()
	if err != nil {
		t.Fatal(err)
	}

	err = SetAuthorized(out[0].Address, out[0].Username)
	if err != nil {
		t.Fatal(err)
	}

	if !IsAuthed(out[0].Address) {
		t.Fatal("after setting user device as authorized it should be.... authorized")
	}

	ip, _, err := net.ParseCIDR(config.GetEffectiveAcl(out[0].Username).Mfa[0])
	if err != nil {
		t.Fatal("could not parse ip: ", err)
	}

	testAuthorizedPacket := ipv4.Header{
		Version: 4,
		Dst:     ip,
		Src:     net.ParseIP(out[0].Address),
		Len:     ipv4.HeaderLen,
	}

	if testAuthorizedPacket.Src == nil || testAuthorizedPacket.Dst == nil {
		t.Fatal("could not parse ip")
	}

	packet, err := testAuthorizedPacket.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	value, _, err := xdpObjects.bpfPrograms.XdpWagFirewall.Test(packet)
	if err != nil {
		t.Fatalf("program failed %s", err)
	}

	if value != 2 {
		t.Fatalf("program did not %s packet instead did: %s", result(2), result(value))
	}

	t.Logf("Waiting for %d minutes to test max session timeout", config.Values().MaxSessionLifetimeMinutes)

	time.Sleep(time.Minute * time.Duration(config.Values().MaxSessionLifetimeMinutes))

	value, _, err = xdpObjects.bpfPrograms.XdpWagFirewall.Test(packet)
	if err != nil {
		t.Fatalf("program failed %s", err)
	}

	if value != 1 {
		t.Fatalf("program did not %s packet instead did: %s", result(1), result(value))
	}

	if IsAuthed(out[0].Address) {
		t.Fatal("user is still authorized after inactivity timeout should have killed them")
	}
}

func TestDisablingMaxLifetime(t *testing.T) {
	if err := setup("../config/test_disabled_max_lifetime.json"); err != nil {
		t.Fatal(err)
	}
	defer xdpObjects.Close()

	out, err := addDevices()
	if err != nil {
		t.Fatal(err)
	}

	err = SetAuthorized(out[0].Address, out[0].Username)
	if err != nil {
		t.Fatal(err)
	}

	if !IsAuthed(out[0].Address) {
		t.Fatal("after setting user as authorized it should be.... authorized")
	}

	var maxSessionLifeDevice fwentry
	deviceBytes, err := xdpObjects.Devices.LookupBytes(net.ParseIP(out[0].Address).To4())
	if err != nil {
		t.Fatal(err)
	}

	err = maxSessionLifeDevice.Unpack(deviceBytes)
	if err != nil {
		t.Fatal(err)
	}

	if maxSessionLifeDevice.sessionExpiry != math.MaxUint64 {
		t.Fatalf("lifetime was not set to max uint64, was %d (maxuint64 %d)", maxSessionLifeDevice.sessionExpiry, uint64(math.MaxUint64))
	}

	ip, _, err := net.ParseCIDR(config.GetEffectiveAcl(out[0].Username).Mfa[0])
	if err != nil {
		t.Fatal("could not parse ip: ", err)
	}

	testAuthorizedPacket := ipv4.Header{
		Version: 4,
		Dst:     ip,
		Src:     net.ParseIP(out[0].Address),
		Len:     ipv4.HeaderLen,
	}

	if testAuthorizedPacket.Src == nil || testAuthorizedPacket.Dst == nil {
		t.Fatal("could not parse ip")
	}

	packet, err := testAuthorizedPacket.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("Now doing timing test for disabled sliding window waiting...")

	elapsed := 0
	for {
		time.Sleep(15 * time.Second)
		elapsed += 15

		t.Logf("waiting %d sec...", elapsed)

		value, _, err := xdpObjects.bpfPrograms.XdpWagFirewall.Test(packet)
		if err != nil {
			t.Fatalf("program failed %s", err)
		}

		if value == 1 {
			t.Fatal("should not block traffic")
		}

		if elapsed > 30 {
			break
		}

	}

}

func TestLookupDifferentKeyTypesInMap(t *testing.T) {
	if err := setup("../config/test_port_based_rules.json"); err != nil {
		t.Fatal(err)
	}
	defer xdpObjects.Close()

	out, err := addDevices()
	if err != nil {
		t.Fatal(err)
	}

	userPublicRoutes, err := getInnerMap(out[0].Username, xdpObjects.PublicTable)
	if err != nil {
		t.Fatal(err)
	}

	// Check negative case
	err = userPublicRoutes.Lookup([]byte("3470239uy4skljhd"), nil)
	if err == nil {
		t.Fatal("searched garbage string, should not match")
	}

	/*
	   "Allow": [
	       "1.1.0.0/16",
	       "2.2.2.2",
	       "3.3.3.3 33/tcp",
	       "4.4.4.4 43/udp",
	       "5.5.5.5 55/any",
	       "6.6.6.6 100-150/tcp"
	   ]
	*/

	// printInner := func(key routetypes.Key, value []byte) (err error) {
	// 	log.Println("looked up:", key.Bytes(), value, key)
	// 	log.Println("contains: ")

	// 	var innerKey []byte
	// 	val := make([]byte, 8)
	// 	innerIter := userPublicRoutes.Iterate()
	// 	for innerIter.Next(&innerKey, &val) {
	// 		var k routetypes.Key

	// 		k.Unpack(innerKey)

	// 		log.Println(innerKey, val, k)
	// 	}

	// 	if innerIter.Err() != nil {
	// 		return innerIter.Err()
	// 	}

	// 	return
	// }

}

// https://stackoverflow.com/questions/36000487/check-for-equality-on-slices-without-order
func sameStringSlice(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}

	// create a map of string -> int
	diff := make(map[string]int, len(x))
	for _, _x := range x {
		// 0 value for int is 0, so just increment a counter for the string
		diff[_x]++
	}
	for _, _y := range y {
		// If the string _y is not in diff bail out early
		if _, ok := diff[_y]; !ok {
			return false
		}
		diff[_y] -= 1
		if diff[_y] == 0 {
			delete(diff, _y)
		}
	}
	return len(diff) == 0
}

func getInnerMap(username string, m *ebpf.Map) (*ebpf.Map, error) {
	var innerMapID ebpf.MapID
	userid := sha1.Sum([]byte(username))

	err := m.Lookup(userid, &innerMapID)
	if err != nil {
		return nil, err
	}

	innerMap, err := ebpf.NewMapFromID(innerMapID)
	if err != nil {
		return nil, fmt.Errorf("failed to get map from id: %s", err)
	}

	return innerMap, nil
}

func checkLPMMap(username string, m *ebpf.Map) ([]string, error) {

	innerMap, err := getInnerMap(username, m)
	if err != nil {
		return nil, err
	}

	result := []string{}

	var innerKey []byte
	var val uint8
	innerIter := innerMap.Iterate()
	kv := routetypes.Key{}
	for innerIter.Next(&innerKey, &val) {
		kv.Unpack(innerKey)

		result = append(result, kv.String())
	}

	if innerIter.Err() != nil {
		return nil, innerIter.Err()
	}

	return result, innerMap.Close()
}

const XDP_DROP = 1
const XDP_PASS = 2

func result(code uint32) string {
	switch code {
	case XDP_DROP:
		return "XDP_DROP"
	case XDP_PASS:
		return "XDP_PASS"
	default:
		return fmt.Sprintf("XDP_UNKNOWN_UNUSED(%d)", code)
	}
}

func addDevices() ([]data.Device, error) {

	devices := []data.Device{
		{
			Address:   "192.168.1.2",
			Publickey: "dc99y+fmhaHwFToSIw/1MSVXewbiyegBMwNGA6LG8yM=",
			Username:  "tester",
			Attempts:  0,
		},
		{
			Address:   "192.168.1.3",
			Publickey: "sXns6f8d6SMehnT6DQG8URCXnNCFe6ouxVmpJB7WeS0=",
			Username:  "randomthingappliedtoall",
			Attempts:  0,
		},
	}

	for i := range devices {
		err := AddUser(devices[i].Username, config.GetEffectiveAcl(devices[i].Username))
		if err != nil {
			return nil, err
		}

		err = xdpAddDevice(devices[i].Username, devices[i].Address)
		if err != nil {
			return nil, err
		}
	}
	return devices, nil
}

func setup(what string) error {
	err := config.Load(what)
	if err != nil && !strings.Contains(err.Error(), "Configuration has already been loaded") {
		return err
	}

	err = data.Load(config.Values().DatabaseLocation)
	if err != nil {
		return err
	}

	return loadXDP()
}
