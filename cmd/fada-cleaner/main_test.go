package main

import "testing"

const sampleMultipath = `3624a9370274318935cf84e2b0202466e dm-9 PURE,FlashArray
size=1.0G features='0' hwhandler='1 alua' wp=rw
` + "`" + `-+- policy='service-time 0' prio=50 status=active
  |- 3:0:0:200 sdk 8:160 active ready running
  ` + "`" + `- 4:0:0:200 sdj 8:144 active ready running
3624a9370274318935cf84e2b020239b8 dm-3 PURE,FlashArray
size=5.0G features='0' hwhandler='1 alua' wp=rw
` + "`" + `-+- policy='service-time 0' prio=50 status=active
  |- 3:0:0:5 sdc 8:32 active ready running
  ` + "`" + `- 4:0:0:5 sdd 8:48 active ready running
36000000000000000000000000000abcd dm-0 OTHER,Disk
size=1.0G features='0' hwhandler='0' wp=rw
  ` + "`" + `- 1:0:0:0 sda 8:0 active ready running
`

func TestParseMultipath(t *testing.T) {
	maps := parseMultipath(sampleMultipath)
	if len(maps) != 3 {
		t.Fatalf("want 3 maps, got %d", len(maps))
	}
	m0 := maps[0]
	if m0.WWID != "3624a9370274318935cf84e2b0202466e" || m0.DM != "dm-9" {
		t.Errorf("map0 header wrong: %+v", m0)
	}
	if m0.Vendor != "PURE,FlashArray" {
		t.Errorf("map0 vendor wrong: %q", m0.Vendor)
	}
	if len(m0.Paths) != 2 || m0.Paths[0] != "sdk" || m0.Paths[1] != "sdj" {
		t.Errorf("map0 paths wrong: %v", m0.Paths)
	}
	if len(maps[1].Paths) != 2 || maps[1].Paths[0] != "sdc" {
		t.Errorf("map1 paths wrong: %v", maps[1].Paths)
	}
	// Non-PURE map is still parsed; vendor filtering happens in the loop.
	if maps[2].Vendor != "OTHER,Disk" {
		t.Errorf("map2 vendor wrong: %q", maps[2].Vendor)
	}
}

func TestParseMultipathEmpty(t *testing.T) {
	if got := parseMultipath(""); len(got) != 0 {
		t.Fatalf("want 0 maps for empty input, got %d", len(got))
	}
}
