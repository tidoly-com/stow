//go:build ignore

// Builds places.tsv.gz from GeoNames (https://www.geonames.org, CC BY 4.0).
//
//	go run gen.go <folder holding cities500.zip, KR.zip, alternatenames_KR.zip, admin1CodesASCII.txt, admin2Codes.txt, countryInfo.txt>
//
// The files come from https://download.geonames.org/export/dump/ (alternatenames_KR.zip is alternatenames/KR.zip).
//
// Outside Korea a point is a town of 500 people or more, named in English; parts of towns are left out. In Korea every populated place
// and neighbourhood is a point, named by its district (시·군·구) in Korean — a photo is rarely more than
// a kilometre or two from one, so the nearest point's district is the photo's district.
package main

import (
	"archive/zip"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func lines(dir, name, member string) [][]string {
	var r io.Reader
	if member == "" {
		f, err := os.Open(filepath.Join(dir, name))
		check(err)
		defer f.Close()
		r = f
	} else {
		z, err := zip.OpenReader(filepath.Join(dir, name))
		check(err)
		defer z.Close()
		f, err := z.Open(member)
		check(err)
		defer f.Close()
		r = f
	}
	var out [][]string
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 1<<20), 1<<24)
	for scan.Scan() {
		if line := scan.Text(); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, strings.Split(line, "\t"))
		}
	}
	check(scan.Err())
	return out
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

type point struct {
	lat, lon            float64
	city, state, nation string
	people              int // 0 in Korea, where the nearest point simply wins
}

func main() {
	dir := os.Args[1]
	korean := map[string]string{} // geoname id -> Korean name, the preferred one if several
	for _, r := range lines(dir, "alternatenames_KR.zip", "KR.txt") {
		if r[2] == "ko" && (korean[r[1]] == "" || r[4] == "1") {
			korean[r[1]] = r[3]
		}
	}
	nation := map[string]string{}
	for _, r := range lines(dir, "countryInfo.txt", "") {
		nation[r[0]] = r[4]
	}
	nation["KR"] = "대한민국"
	state, district := map[string]string{}, map[string]string{}
	for _, r := range lines(dir, "admin1CodesASCII.txt", "") {
		state[r[0]] = r[1]
		if ko := korean[r[3]]; strings.HasPrefix(r[0], "KR.") && ko != "" {
			state[r[0]] = ko
		}
	}
	for _, r := range lines(dir, "admin2Codes.txt", "") {
		if ko := korean[r[3]]; strings.HasPrefix(r[0], "KR.") && ko != "" {
			district[r[0]] = ko
		}
	}

	var points []point
	num := func(s string) float64 { v, err := strconv.ParseFloat(s, 64); check(err); return v }
	for _, r := range lines(dir, "cities500.zip", "cities500.txt") {
		if r[8] == "KR" || r[7] == "PPLX" { // PPLX is a part of a town: "Paris 16 Passy" where a person would say Paris
			continue
		}
		people, _ := strconv.Atoi(r[14])
		points = append(points, point{num(r[4]), num(r[5]), r[1], state[r[8]+"."+r[10]], nation[r[8]], people})
	}
	for _, r := range lines(dir, "KR.zip", "KR.txt") {
		populated := r[6] == "P" || r[6] == "A" && (r[7] == "ADM2" || r[7] == "ADM3" || r[7] == "ADM4")
		name := district["KR."+r[10]+"."+r[11]]
		if !populated || name == "" {
			continue
		}
		points = append(points, point{num(r[4]), num(r[5]), name, state["KR."+r[10]], nation["KR"], 0})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].lat < points[j].lat })

	out, err := os.Create("places.tsv.gz")
	check(err)
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	w := bufio.NewWriter(zw)
	// regions first, then points that refer to them by number: "R state nation" / "lat lon city region people"
	region := map[string]int{}
	for _, p := range points {
		key := p.state + "\t" + p.nation
		if _, seen := region[key]; !seen {
			region[key] = len(region)
			fmt.Fprintf(w, "R\t%s\n", key)
		}
	}
	for _, p := range points {
		fmt.Fprintf(w, "%.4f\t%.4f\t%s\t%d\t%d\n", p.lat, p.lon, p.city, region[p.state+"\t"+p.nation], p.people)
	}
	check(w.Flush())
	check(zw.Close())
	check(out.Close())
	fmt.Println(len(points), "points,", len(region), "regions")
}
