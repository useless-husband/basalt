// Package slt runs files from SQLite's sqllogictest corpus against basalt
// and reports how many records pass.
//
// The corpus is downloaded at test time (from the GitHub mirror at a fixed
// commit) into testdata/sqllogictest/, which is not committed.
//
//	BASALT_SLT=1 go test -run TestSQLLogic -v ./test/slt/
//	BASALT_SLT_FILES='test/select1.test,test/evidence/*.test' ...  (subset)
//
// Records marked "skipif postgresql" are skipped and "onlyif" records for
// other engines are ignored, exactly as the corpus intends for a
// PostgreSQL-compatible engine.
package slt

import (
	"bufio"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useless-husband/basalt/internal/engine"
	"github.com/useless-husband/basalt/internal/types"
	"github.com/useless-husband/basalt/internal/vfs"
)

const (
	corpusCommit = "c67f97bf3ca7e590d12e073408bcacaf2ff0f3a0"
	corpusURL    = "https://raw.githubusercontent.com/gregrahn/sqllogictest/" + corpusCommit + "/"
)

// defaultFiles is the documented subset: the five select files, the
// evidence files, and the first ten files of each random/* family.
func defaultFiles() []string {
	files := []string{"test/select1.test", "test/select2.test", "test/select3.test", "test/select4.test", "test/select5.test"}
	for _, e := range []string{"in1", "in2", "slt_lang_aggfunc", "slt_lang_createtrigger", "slt_lang_createview",
		"slt_lang_dropindex", "slt_lang_droptable", "slt_lang_droptrigger", "slt_lang_dropview", "slt_lang_reindex",
		"slt_lang_replace", "slt_lang_update"} {
		files = append(files, "test/evidence/"+e+".test")
	}
	for _, fam := range []string{"aggregates", "expr", "groupby", "select"} {
		for i := 0; i < 10; i++ {
			files = append(files, fmt.Sprintf("test/random/%s/slt_good_%d.test", fam, i))
		}
	}
	return files
}

func fetch(t *testing.T, rel string) string {
	t.Helper()
	local := filepath.Join("..", "..", "testdata", "sqllogictest", corpusCommit, rel)
	if _, err := os.Stat(local); err == nil {
		return local
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(corpusURL + rel)
	if err != nil {
		t.Fatalf("download %s: %v", rel, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download %s: HTTP %d", rel, resp.StatusCode)
	}
	tmp := local + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if err := os.Rename(tmp, local); err != nil {
		t.Fatal(err)
	}
	return local
}

type record struct {
	kind      string // statement or query
	expectErr bool
	types     string
	sortMode  string
	sql       string
	expected  []string
	line      int
}

type result struct {
	file                    string
	pass, fail, skip, total int
	failures                map[string]int // first words of error -> count
	examples                []string
}

// parse reads a test file into records, applying skipif/onlyif.
func parse(path string) ([]record, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	var recs []record
	skipped := 0
	skip := false
	for i := 0; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], " \t\r")
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		fields := strings.Fields(l)
		switch fields[0] {
		case "skipif":
			if len(fields) > 1 && fields[1] == "postgresql" {
				skip = true
			}
			continue
		case "onlyif":
			if len(fields) > 1 && fields[1] != "postgresql" {
				skip = true
			}
			continue
		case "hash-threshold", "halt":
			if fields[0] == "halt" && !skip {
				return recs, skipped, nil
			}
			skip = false
			continue
		case "statement", "query":
		default:
			continue
		}
		r := record{kind: fields[0], line: i + 1}
		if r.kind == "statement" {
			r.expectErr = len(fields) > 1 && fields[1] == "error"
		} else {
			if len(fields) > 1 {
				r.types = fields[1]
			}
			r.sortMode = "nosort"
			if len(fields) > 2 {
				r.sortMode = fields[2]
			}
		}
		var sqlLines []string
		i++
		for ; i < len(lines); i++ {
			l := strings.TrimRight(lines[i], "\r")
			if l == "" || l == "----" {
				break
			}
			sqlLines = append(sqlLines, l)
		}
		r.sql = strings.Join(sqlLines, "\n")
		if r.kind == "query" && i < len(lines) && strings.TrimRight(lines[i], "\r") == "----" {
			i++
			for ; i < len(lines); i++ {
				l := strings.TrimRight(lines[i], "\r")
				if l == "" {
					break
				}
				r.expected = append(r.expected, l)
			}
		}
		if skip {
			skipped++
			skip = false
			continue
		}
		recs = append(recs, r)
	}
	return recs, skipped, nil
}

// format renders a value the way sqllogictest expects.
func format(v types.Value, t types.T, typ byte) string {
	if v.IsNull() {
		return "NULL"
	}
	switch typ {
	case 'I':
		switch v.K {
		case types.KInt:
			return strconv.FormatInt(v.I, 10)
		case types.KFloat, types.KNumeric:
			return strconv.FormatInt(int64(math.Trunc(v.AsFloat())), 10)
		case types.KBool:
			if v.Bool() {
				return "1"
			}
			return "0"
		case types.KText:
			if n, err := strconv.ParseFloat(strings.TrimSpace(v.S), 64); err == nil {
				return strconv.FormatInt(int64(math.Trunc(n)), 10)
			}
			return "0"
		}
	case 'R':
		switch v.K {
		case types.KInt, types.KFloat, types.KNumeric:
			return fmt.Sprintf("%.3f", v.AsFloat())
		case types.KText:
			if n, err := strconv.ParseFloat(strings.TrimSpace(v.S), 64); err == nil {
				return fmt.Sprintf("%.3f", n)
			}
			return "0.000"
		}
	}
	s := types.ToText(v, t)
	if s == "" {
		return "(empty)"
	}
	b := []byte(s)
	for i, c := range b {
		if c < ' ' || c > '~' {
			b[i] = '@'
		}
	}
	return string(b)
}

func hashValues(vals []string) string {
	h := md5.New()
	for _, v := range vals {
		io.WriteString(h, v)
		io.WriteString(h, "\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func runFile(t *testing.T, path, name string) result {
	res := result{file: name, failures: map[string]int{}}
	recs, skipped, err := parse(path)
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return res
	}
	res.skip = skipped
	dir := t.TempDir()
	db, err := engine.Open(engine.Options{Dir: dir, FS: vfs.NoSync{FS: vfs.OS{}}, PoolPages: 4096, CheckpointInterval: -1})
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return res
	}
	defer db.Close()
	s := db.NewSession("slt", "slt")
	defer s.Close()
	threshold := 8
	fail := func(r record, why string) {
		res.fail++
		key := why
		if len(key) > 60 {
			key = key[:60]
		}
		res.failures[key]++
		if len(res.examples) < 5 {
			res.examples = append(res.examples, fmt.Sprintf("%s:%d: %s\n    %s", name, r.line, why, strings.ReplaceAll(r.sql, "\n", " ")))
		}
	}
	for _, r := range recs {
		res.total++
		results, err := s.Exec(r.sql)
		if r.kind == "statement" {
			switch {
			case r.expectErr && err != nil, !r.expectErr && err == nil:
				res.pass++
			case r.expectErr:
				fail(r, "statement should have failed")
			default:
				fail(r, "error: "+err.Error())
			}
			continue
		}
		if err != nil {
			fail(r, "error: "+err.Error())
			continue
		}
		last := results[len(results)-1]
		if len(r.types) > 0 && len(last.Columns) != len(r.types) {
			fail(r, fmt.Sprintf("wrong column count %d", len(last.Columns)))
			continue
		}
		var rows [][]string
		for _, row := range last.Rows {
			var vals []string
			for i, v := range row {
				typ := byte('T')
				if i < len(r.types) {
					typ = r.types[i]
				}
				vals = append(vals, format(v, last.Columns[i].Type, typ))
			}
			rows = append(rows, vals)
		}
		switch r.sortMode {
		case "rowsort":
			sort.Slice(rows, func(i, j int) bool { return strings.Join(rows[i], " ") < strings.Join(rows[j], " ") })
		}
		var flat []string
		for _, row := range rows {
			flat = append(flat, row...)
		}
		if r.sortMode == "valuesort" {
			sort.Strings(flat)
		}
		var got []string
		if len(r.expected) == 1 && strings.Contains(r.expected[0], "values hashing to") {
			got = []string{fmt.Sprintf("%d values hashing to %s", len(flat), hashValues(flat))}
		} else if threshold > 0 && len(flat) > threshold && len(r.expected) == 1 {
			got = []string{fmt.Sprintf("%d values hashing to %s", len(flat), hashValues(flat))}
		} else {
			got = flat
		}
		if strings.Join(got, "\n") == strings.Join(r.expected, "\n") {
			res.pass++
			continue
		}
		// Expected output may also be written one row per line.
		var rowLines []string
		for _, row := range rows {
			rowLines = append(rowLines, strings.Join(row, " "))
		}
		if strings.Join(rowLines, "\n") == strings.Join(r.expected, "\n") {
			res.pass++
			continue
		}
		fail(r, "wrong result")
	}
	return res
}

func TestSQLLogic(t *testing.T) {
	if os.Getenv("BASALT_SLT") == "" {
		t.Skip("set BASALT_SLT=1 to run the sqllogictest corpus (downloads ~60 MB)")
	}
	files := defaultFiles()
	if v := os.Getenv("BASALT_SLT_FILES"); v != "" {
		files = strings.Split(v, ",")
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, fetch(t, f))
	}
	results := make([]result, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range files {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			results[i] = runFile(t, paths[i], files[i])
			t.Logf("%-44s %6d/%6d passed (%5.1f%%), %d skipped, %v", files[i], results[i].pass, results[i].total,
				100*float64(results[i].pass)/math.Max(1, float64(results[i].total)), results[i].skip, time.Since(start).Round(time.Second))
		}(i)
	}
	wg.Wait()
	var pass, total, skip int
	why := map[string]int{}
	byGroup := map[string][2]int{}
	for _, r := range results {
		pass += r.pass
		total += r.total
		skip += r.skip
		for k, v := range r.failures {
			why[k] += v
		}
		g := filepath.Dir(r.file)
		x := byGroup[g]
		x[0] += r.pass
		x[1] += r.total
		byGroup[g] = x
	}
	var groups []string
	for g := range byGroup {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		x := byGroup[g]
		t.Logf("SUMMARY %-28s %7d/%7d records passed (%.2f%%)", g, x[0], x[1], 100*float64(x[0])/math.Max(1, float64(x[1])))
	}
	t.Logf("SUMMARY total %d/%d records passed (%.2f%%), %d records skipped by skipif/onlyif", pass, total, 100*float64(pass)/math.Max(1, float64(total)), skip)
	type kv struct {
		k string
		v int
	}
	var reasons []kv
	for k, v := range why {
		reasons = append(reasons, kv{k, v})
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i].v > reasons[j].v })
	for i, r := range reasons {
		if i >= 25 {
			break
		}
		t.Logf("FAILURE %6d  %s", r.v, r.k)
	}
	for _, r := range results {
		for _, ex := range r.examples {
			t.Logf("EXAMPLE %s", ex)
		}
	}
}
