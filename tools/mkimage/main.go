// mkimage baut aus dem statischen PingWacht-Binary ein Container-Image im Format von
// „docker save“ (z. B. für MikroTik RouterOS /container, Docker, Podman) – ohne Docker.
//
//	go run ./tools/mkimage -bin pingwatch-agent-linux-arm64 -arch arm64 -out pingwacht-agent-arm64.tar
package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"
)

func main() {
	bin := flag.String("bin", "", "statisches Linux-Binary")
	arch := flag.String("arch", "arm64", "arm64, arm, amd64")
	variant := flag.String("variant", "", "z. B. v7 bei arm")
	certs := flag.String("certs", "/etc/ssl/certs/ca-certificates.crt", "CA-Zertifikate")
	out := flag.String("out", "", "Ziel (.tar)")
	tag := flag.String("tag", "pingwacht/agent:latest", "Name")
	flag.Parse()
	exe, err := os.ReadFile(*bin)
	if err != nil {
		log.Fatal(err)
	}
	ca, err := os.ReadFile(*certs)
	if err != nil {
		log.Fatal(err)
	}
	mt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Layer: /app/pingwatch, /etc/ssl/certs, /data, /tmp
	var lb bytes.Buffer
	lt := tar.NewWriter(&lb)
	dir := func(n string, mode int64) {
		lt.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeDir, Mode: mode, ModTime: mt})
	}
	file := func(n string, b []byte, mode int64) {
		lt.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(b)), ModTime: mt})
		lt.Write(b)
	}
	for _, d := range []string{"app/", "data/", "etc/", "etc/ssl/", "etc/ssl/certs/"} {
		dir(d, 0o755)
	}
	dir("tmp/", 0o1777)
	file("etc/ssl/certs/ca-certificates.crt", ca, 0o644)
	file("etc/passwd", []byte("root:x:0:0:root:/root:/sbin/nologin\n"), 0o644)
	file("app/pingwatch", exe, 0o755)
	lt.Close()
	layer := lb.Bytes()
	ls := sha256.Sum256(layer)
	lid := hex.EncodeToString(ls[:])
	cfg := map[string]any{
		"architecture": *arch, "os": "linux", "created": mt.Format(time.RFC3339),
		"config": map[string]any{
			"Entrypoint": []string{"/app/pingwatch", "agent", "-data", "/data"},
			"Env":        []string{"PATH=/app", "HOME=/data", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt", "PINGWACHT_CONTAINER=1", "TZ=Europe/Berlin"},
			"WorkingDir": "/data",
			"Volumes":    map[string]any{"/data": map[string]any{}},
		},
		"rootfs":  map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + lid}},
		"history": []map[string]any{{"created": mt.Format(time.RFC3339), "created_by": "PingWacht mkimage"}},
	}
	if *variant != "" {
		cfg["variant"] = *variant
	}
	cb, _ := json.Marshal(cfg)
	cs := sha256.Sum256(cb)
	cid := hex.EncodeToString(cs[:])
	man, _ := json.Marshal([]map[string]any{{"Config": cid + ".json", "RepoTags": []string{*tag}, "Layers": []string{lid + "/layer.tar"}}})
	repos, _ := json.Marshal(map[string]map[string]string{"pingwacht/agent": {"latest": lid}})

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	t := tar.NewWriter(f)
	add := func(n string, b []byte) {
		t.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b)), ModTime: mt})
		t.Write(b)
	}
	t.WriteHeader(&tar.Header{Name: lid + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mt})
	add(lid+"/VERSION", []byte("1.0"))
	add(lid+"/json", []byte(`{"id":"`+lid+`"}`))
	add(lid+"/layer.tar", layer)
	add(cid+".json", cb)
	add("manifest.json", man)
	add("repositories", repos)
	if err := t.Close(); err != nil {
		log.Fatal(err)
	}
	f.Close()
	log.Printf("%s: %d MB", *out, len(layer)>>20)
}
