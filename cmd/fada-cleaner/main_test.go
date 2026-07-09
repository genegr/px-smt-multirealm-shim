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
	// user_friendly_names off → the dm map name equals the WWID.
	if m0.Name != "3624a9370274318935cf84e2b0202466e" {
		t.Errorf("map0 name wrong: %q", m0.Name)
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

// FC hosts (and any host with user_friendly_names/aliases) emit the header as
// "<alias> (<wwid>) dm-N vendor". The map name is the alias, not the WWID — the parser must
// capture both so open-count/flush key off the name while scsi_id keys off the WWID.
const sampleMultipathAlias = `mpatha (3624a9370274318935cf84e2b0202466e) dm-9 PURE,FlashArray
size=1.0G features='1 queue_if_no_path' hwhandler='1 alua' wp=rw
` + "`" + `-+- policy='service-time 0' prio=50 status=active
  |- 1:0:0:1 sdb 8:16 active ready running
  |- 1:0:1:1 sdc 8:32 active ready running
  |- 2:0:0:1 sdd 8:48 active ready running
  ` + "`" + `- 2:0:1:1 sde 8:64 active ready running
`

func TestParseMultipathAlias(t *testing.T) {
	maps := parseMultipath(sampleMultipathAlias)
	if len(maps) != 1 {
		t.Fatalf("want 1 map, got %d", len(maps))
	}
	m := maps[0]
	if m.Name != "mpatha" {
		t.Errorf("alias name wrong: %q", m.Name)
	}
	if m.WWID != "3624a9370274318935cf84e2b0202466e" {
		t.Errorf("alias wwid wrong: %q", m.WWID)
	}
	if m.DM != "dm-9" || m.Vendor != "PURE,FlashArray" {
		t.Errorf("alias header wrong: %+v", m)
	}
	if len(m.Paths) != 4 || m.Paths[0] != "sdb" || m.Paths[3] != "sde" {
		t.Errorf("alias paths wrong: %v", m.Paths)
	}
}
