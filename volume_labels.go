package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Volumes with their labels, for the controller: a backup job can choose volumes
// by stack (Compose's com.docker.compose.project label) or by a label of your own.

const (
	maxVolumeLabels     = 64
	maxVolumeLabelKey   = 128
	maxVolumeLabelValue = 256
)

// dockerVolumesSnapshot lists the volumes, with their labels. The engine API
// gives the labels as a dictionary; `docker volume ls` only prints them as one
// text (a=b,c=d), which is ambiguous when a value holds a comma, so it is the
// fallback.
func dockerVolumesSnapshot() ([]volumeReport, error) {
	if sock := dockerSocket(); sock != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if vols, err := engineVolumes(ctx, unixClient(sock), "http://docker"); err == nil {
			return vols, nil
		}
	}
	return cliVolumes()
}

func engineVolumes(ctx context.Context, client *http.Client, base string) ([]volumeReport, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/volumes", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the engine answered %s", resp.Status)
	}
	var body struct {
		Volumes []struct {
			Name   string            `json:"Name"`
			Driver string            `json:"Driver"`
			Labels map[string]string `json:"Labels"`
		} `json:"Volumes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	reports := make([]volumeReport, 0, len(body.Volumes))
	for _, v := range body.Volumes {
		reports = append(reports, volumeReport{Name: v.Name, Driver: v.Driver, Labels: capLabels(v.Labels)})
	}
	return reports, nil
}

func cliVolumes() ([]volumeReport, error) {
	out, err := exec.Command("docker", "volume", "ls", "--format", "{{json .}}").Output()
	if err != nil {
		return nil, err
	}
	var reports []volumeReport
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw struct {
			Name   string `json:"Name"`
			Driver string `json:"Driver"`
			Labels string `json:"Labels"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue // one malformed line shouldn't drop the whole snapshot
		}
		reports = append(reports, volumeReport{Name: raw.Name, Driver: raw.Driver, Labels: capLabels(parseLabelText(raw.Labels))})
	}
	return reports, scanner.Err()
}

// parseLabelText reads docker's "a=b,c=d". A piece without "=" belongs to the
// value before it: that is how a comma inside a value shows up.
func parseLabelText(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	labels := map[string]string{}
	last := ""
	for _, piece := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(piece, "="); ok && k != "" {
			labels[k], last = v, k
		} else if last != "" {
			labels[last] += "," + piece
		}
	}
	return labels
}

// capLabels keeps what a snapshot can carry without growing without bound.
func capLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if len(out) >= maxVolumeLabels || len(k) > maxVolumeLabelKey {
			continue
		}
		if len(v) > maxVolumeLabelValue {
			v = v[:maxVolumeLabelValue]
		}
		out[k] = v
	}
	return out
}
