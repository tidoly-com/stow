package places

import (
	"testing"
	"time"
)

func TestKnownSpots(t *testing.T) {
	for _, c := range []struct {
		name     string
		lat, lon float64
		want     Place
	}{
		{"서울시청", 37.5663, 126.9779, Place{"중구", "서울특별시", "대한민국"}},
		{"강남역", 37.4979, 127.0276, Place{"강남구", "서울특별시", "대한민국"}},
		{"해운대 해수욕장", 35.1587, 129.1604, Place{"해운대구", "부산광역시", "대한민국"}},
		{"성산일출봉", 33.4581, 126.9425, Place{"서귀포시", "제주특별자치도", "대한민국"}},
		{"경주 불국사", 35.7900, 129.3320, Place{"경주시", "경상북도", "대한민국"}},
		{"전주 한옥마을", 35.8150, 127.1530, Place{"전주시", "전라북도", "대한민국"}},
		{"수원 화성", 37.2870, 127.0120, Place{"수원시", "경기도", "대한민국"}},
		{"인천공항", 37.4602, 126.4407, Place{"중구", "인천광역시", "대한민국"}},
		{"속초 해변", 38.1900, 128.6030, Place{"속초시", "강원도", "대한민국"}},
		{"대전역", 36.3320, 127.4340, Place{"동구", "대전광역시", "대한민국"}},
		// in the largest cities the name that comes back is often a district of the city; region and country hold
		{"Eiffel Tower", 48.8584, 2.2945, Place{"", "Île-de-France", "France"}},
		{"Shibuya crossing", 35.6595, 139.7005, Place{"", "Tokyo", "Japan"}},
		{"Times Square", 40.7580, -73.9855, Place{"", "New York", "United States"}},
		{"downtown Jersey City, across the river from Manhattan", 40.7282, -74.0776, Place{"Jersey City", "New Jersey", "United States"}},
		{"a village 20 km from Florence", 43.6100, 11.4500, Place{"", "Tuscany", "Italy"}},
		{"Sydney Opera House", -33.8568, 151.2153, Place{"Sydney", "New South Wales", "Australia"}},
		{"Arezzo, where the Nikon sample was taken", 43.4674, 11.8851, Place{"Arezzo", "Tuscany", "Italy"}},
	} {
		got, ok := Lookup(c.lat, c.lon)
		if !ok || got.State != c.want.State || got.Country != c.want.Country || (c.want.City != "" && got.City != c.want.City) {
			t.Errorf("%s: %+v (found: %v), want %+v", c.name, got, ok, c.want)
		} else {
			t.Logf("%s: %s, %s, %s", c.name, got.City, got.State, got.Country)
		}
	}
}

func TestNowhere(t *testing.T) {
	for name, at := range map[string][2]float64{"mid Pacific": {0, -150}, "East Sea, between Korea and Japan": {37.5, 132.5}, "South Pole": {-90, 0}} {
		if got, ok := Lookup(at[0], at[1]); ok {
			t.Errorf("%s: %+v, want nothing", name, got)
		}
	}
}

func TestQuickEnoughToRunForEveryPhoto(t *testing.T) {
	Lookup(0, 0)
	start := time.Now()
	for i := 0; i < 1000; i++ {
		Lookup(37.5+float64(i)/1000, 127)
	}
	if per := time.Since(start) / 1000; per > 5*time.Millisecond {
		t.Errorf("%v per lookup", per)
	} else {
		t.Logf("%v per lookup", per)
	}
}
