package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const crushNone = 2147483647

// Mapper считает раскладку PG штатными osdmaptool и crushtool (локально или в контейнере Ceph).
// В симуляции не участвует: нужен `cephsim verify`, чтобы сверить с ним Go-порт CRUSH.
type Mapper struct {
	Bin       string // каталог с osdmaptool и crushtool; пусто - искать в PATH, иначе docker
	Image     string
	ExtraArgs []string
}

type Mapping map[string][]int

func (m Mapper) command(dir, script string) *exec.Cmd {
	if m.Bin != "" {
		script = "export PATH=" + m.Bin + ":$PATH\n" + script
	}
	_, e1 := exec.LookPath("osdmaptool")
	_, e2 := exec.LookPath("crushtool")
	if m.Bin != "" || (e1 == nil && e2 == nil) {
		cmd := exec.Command("bash", "-c", script)
		cmd.Dir = dir
		return cmd
	}
	return exec.Command("docker", "run", "--rm", "-v", dir+":/w", "-w", "/w", "--entrypoint", "bash", m.Image, "-c", script)
}

// Map считает up-set каждой PG. OSD из remove убираются из копии CRUSH-карты:
// для straw2 это то же, что CRUSH-вес 0, и не сбрасывается флагом --mark-up-in.
// crushtool принимает один --remove-item за вызов, поэтому цикл.
func (m Mapper) Map(osdmap string, remove []int, extra ...string) (Mapping, error) {
	dir, err := os.MkdirTemp("", "cephsim-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := copyFile(osdmap, filepath.Join(dir, "om.bin")); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	if len(remove) > 0 {
		b.WriteString("osdmaptool om.bin --export-crush crush.bin >/dev/null 2>&1\n")
		for _, id := range remove {
			fmt.Fprintf(&b, "crushtool -i crush.bin --remove-item osd.%d -o crush.bin >/dev/null 2>&1\n", id)
		}
		b.WriteString("osdmaptool om.bin --import-crush crush.bin >/dev/null 2>&1\n")
	}
	// -dump печатает acting (с pg_temp во время backfill), -dump-all - ещё и up: сверяем up.
	fmt.Fprintf(&b, "osdmaptool om.bin %s --test-map-pgs-dump-all 2>/dev/null\n", strings.Join(append(append([]string{}, m.ExtraArgs...), extra...), " "))

	cmd := m.command(dir, b.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("osdmaptool/crushtool: %w\n%s", err, stderr.String())
	}
	return parseMapping(&stdout)
}

var (
	mapLine = regexp.MustCompile(`^(\d+\.[0-9a-f]+)\s+\[([0-9,]*)\]\s+(-?\d+)`)
	// --test-map-pgs-dump-all: "1.0 raw ([..], p3) up ([..], p3) acting ([..], p3)"
	upLine = regexp.MustCompile(`^(\d+\.[0-9a-f]+) raw \(\[[0-9,]*\], p-?\d+\) up \(\[([0-9,]*)\]`)
)

func parseMapping(r io.Reader) (Mapping, error) {
	res := Mapping{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		mm := upLine.FindStringSubmatch(sc.Text())
		if mm == nil {
			mm = mapLine.FindStringSubmatch(sc.Text())
		}
		if mm == nil {
			continue
		}
		var osds []int
		if mm[2] != "" {
			for _, f := range strings.Split(mm[2], ",") {
				id, err := strconv.Atoi(f)
				if err != nil {
					return nil, err
				}
				osds = append(osds, id)
			}
		}
		res[mm[1]] = osds
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("osdmaptool не вернул ни одной PG")
	}
	return res, sc.Err()
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
