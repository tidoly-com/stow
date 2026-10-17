// Package places turns coordinates into a place name without asking anyone: the names travel inside the
// program. Data from GeoNames (https://www.geonames.org), licensed CC BY 4.0; gen.go builds the file.
package places

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed places.tsv.gz
var packed []byte

// Place is where a photo was taken, as far as the nearest known settlement tells.
type Place struct{ City, State, Country string }

type point struct {
	lat, lon float32
	city     string
	region   int32
	pull     float32 // grows with the number of people living there
}

var (
	once    sync.Once
	points  []point // sorted by latitude
	regions [][2]string
)

func load() {
	zr, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		panic(err)
	}
	scan := bufio.NewScanner(zr)
	for scan.Scan() {
		f := strings.Split(scan.Text(), "\t")
		if f[0] == "R" {
			regions = append(regions, [2]string{f[1], f[2]})
			continue
		}
		lat, _ := strconv.ParseFloat(f[0], 32)
		lon, _ := strconv.ParseFloat(f[1], 32)
		region, _ := strconv.Atoi(f[3])
		people, _ := strconv.Atoi(f[4])
		pull := math.Log10(float64(people) + 10)
		points = append(points, point{float32(lat), float32(lon), f[2], int32(region), float32(pull * pull)})
	}
}

// MaxDistanceKm is how far the nearest settlement may be before a photo counts as taken nowhere in
// particular — out at sea, or deep in the mountains.
const MaxDistanceKm = 25

// Lookup names the settlement the coordinates belong to, if one lies within MaxDistanceKm. It is the
// nearest one, except that a big town outweighs a hamlet that happens to be closer: a photo taken at
// Times Square is filed under New York, not under the small town across the river.
func Lookup(lat, lon float64) (Place, bool) {
	once.Do(load)
	const kmPerDegree = 111.2
	window := float32(MaxDistanceKm / kmPerDegree)
	from := sort.Search(len(points), func(i int) bool { return points[i].lat >= float32(lat)-window })
	shrink := math.Cos(lat * math.Pi / 180) // a degree of longitude is shorter away from the equator
	best, bestScore := -1, math.MaxFloat64
	for i := from; i < len(points) && points[i].lat <= float32(lat)+window; i++ {
		dLon := math.Abs(float64(points[i].lon) - lon)
		if dLon > 180 {
			dLon = 360 - dLon
		}
		km := kmPerDegree * math.Hypot(float64(points[i].lat)-lat, dLon*shrink)
		if score := km / float64(points[i].pull); km <= MaxDistanceKm && score < bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return Place{}, false
	}
	r := regions[points[best].region]
	return Place{City: points[best].city, State: r[0], Country: r[1]}, true
}
