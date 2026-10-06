package state

import "sort"

// SetSlots records which slots of a service are deployed, in slot order.
// Exactly the implicit state -- slot 1 alone, at the deployment's own
// version -- is not recorded at all, so a deployment that never had
// replicas (or is back to one) reads and writes the same as before slots
// existed.
func (s *State) SetSlots(service string, slots []Slot) {
	slots = append([]Slot(nil), slots...)
	sort.Slice(slots, func(i, j int) bool { return slots[i].N < slots[j].N })
	if len(slots) == 1 && slots[0].N == 1 && slots[0].Version == s.Version {
		delete(s.Slots, service)
		if len(s.Slots) == 0 {
			s.Slots = nil
		}
		return
	}
	if s.Slots == nil {
		s.Slots = map[string][]Slot{}
	}
	s.Slots[service] = slots
}

// SaveSlots records a service's slots in state.json. Only that field
// changes: the record is read again here, not taken from a caller's copy,
// like MarkStarting and MarkRunning. The caller holds Lock.
func SaveSlots(service string, slots []Slot) error {
	s, err := Load()
	if err != nil {
		return err
	}
	s.SetSlots(service, slots)
	return s.Save()
}

// RunsFromCurrentBundle reports whether the containers are known to run from
// this record's own bundle: the last `up` finished (MarkRunning leaves just
// that bundle). Between a configure and the next `up` the old bundle's
// containers are still the ones serving, and files written into the new
// bundle would not reach them. A legacy layout (no bundle directory) has
// only one place files can be.
func (s *State) RunsFromCurrentBundle() bool {
	if s.BundleDir == "" {
		return true
	}
	return len(s.MountedBundleDirs) == 1 && s.MountedBundleDirs[0] == s.BundleDir
}
