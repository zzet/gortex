//go:build acceptance

package main

import "errors"

func readbackReferenceHostIdentity(snapshot readbackReferenceHostSnapshot) (string, error) {
	if snapshot.Error != "" || snapshot.VolumeIdentity == "" {
		return "", errors.New("host or owning-volume identity unavailable")
	}
	return hashJSON(struct {
		Host           Host   `json:"host"`
		VolumeIdentity string `json:"volume_identity"`
	}{snapshot.Host, snapshot.VolumeIdentity})
}

func compareReadbackReferenceHosts(before, after readbackReferenceHostSnapshot) error {
	a, errA := readbackReferenceHostIdentity(before)
	b, errB := readbackReferenceHostIdentity(after)
	if errA != nil || errB != nil || a != b {
		return errors.New("actual reference host identity/limits or owning volume changed during phase")
	}
	return nil
}
