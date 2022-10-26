// database.go
// Copyright(c) 2022 Matt Pharr, licensed under the GNU Public License, Version 3.
// SPDX: GPL-3.0-only

package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/mmp/earcut-go"
	"github.com/mmp/sct2"
)

// StaticDatabase is a catch-all for data about the world that doesn't
// change after it's loaded.  It includes information from FAA databases,
// the sector file, and the position file.
type StaticDatabase struct {
	// From the FAA (et al.) databases
	FAA struct {
		navaids  map[string]Navaid
		airports map[string]Airport
		fixes    map[string]Fix
		prd      map[AirportPair][]PRDEntry
	}
	callsigns map[string]Callsign

	// From the sector file
	NmPerLatitude     float32
	NmPerLongitude    float32
	MagneticVariation float32

	defaultAirport string
	defaultCenter  Point2LL
	sectorFileId   string

	VORs     map[string]Point2LL
	NDBs     map[string]Point2LL
	fixes    map[string]Point2LL
	airports map[string]Point2LL
	runways  map[string][]Runway

	sectorFileColors map[string]RGB

	sectorFileLoadError error

	// Static things to draw; derived from the sector file
	//
	// These only store geometry; no colors; the caller should set current
	// RGB based on the active color scheme.
	runwayCommandBuffer               CommandBuffer
	lowAirwayCommandBuffer            CommandBuffer
	highAirwayCommandBuffer           CommandBuffer
	regions                           []StaticDrawable
	ARTCC                             []StaticDrawable
	ARTCCLow                          []StaticDrawable
	ARTCCHigh                         []StaticDrawable
	geos                              []StaticDrawable
	SIDs, STARs                       []StaticDrawable
	lowAirwayLabels, highAirwayLabels []Label
	labelColorBufferIndex             ColorBufferIndex
	labels                            []Label

	// From the position file
	positions             map[string][]Position // map key is e.g. JFK_TWR
	positionFileLoadError error
}

// Label represents a labeled point on a map.
type Label struct {
	name  string
	p     Point2LL
	color RGB
}

// StaticDrawable represents a fixed object (e.g., a region of the map, a
// SID or STAR, etc.) that can be drawn.
type StaticDrawable struct {
	name     string
	cb       CommandBuffer
	rgbSlice []float32
	// Bounding box in latitude-longitude coordinates
	bounds           Extent2D
	colorBufferIndex ColorBufferIndex
}

// ColorBufferIndex provides an efficient encoding of named sections of an
// RGB buffer used for rendering. More to the point, sector files include
// various objects that defined by lines where each line has a string
// associated with it that names it for color assignment during rendering
// (e.g., "RWY", "TAXI", etc.). We don't want to store a string for each
// line segment, we would like to maintain a pre-generated buffer of RGB
// values for these segments for the GPU, and we would like to be able to
// efficiently update these RGBs when the user assigns a new color for
// "TAXI" or what have you.  Thence, ColorBufferIndex.
type ColorBufferIndex struct {
	// Each color name has an integer identifier associated with it.
	m map[string]int
	// And then each line segment has an integer associated with it that
	// corresponds to its original color name.
	ids []int
}

func NewColorBufferIndex() ColorBufferIndex { return ColorBufferIndex{m: make(map[string]int)} }

// Add should be called whenever a color name is encountered when
// processing lines in a sector file. It handles the housekeeping needed to
// associate the name with the current line segment.
func (c *ColorBufferIndex) Add(name string) {
	if id, ok := c.m[name]; ok {
		// Seen it already; associate the name with the current line
		// segment.
		c.ids = append(c.ids, id)
	} else {
		// First time: generate an identifier for the name, record it, and
		// associate it with the line segment.
		newId := len(c.m) + 1
		c.m[name] = newId
		c.ids = append(c.ids, newId)
	}
}

// Visit should be called when a named color has been changed; the provided
// callback function is then called with the segment index of each line
// segment that was associated with the specified name.
func (c *ColorBufferIndex) Visit(name string, callback func(int)) {
	if matchId, ok := c.m[name]; !ok {
		// This name isn't associated with any line segments.
		return
	} else {
		// Loop over all of the segments and then call the callback for
		// each one that is associated with |name|.
		for i, id := range c.ids {
			if id == matchId {
				callback(i)
			}
		}
	}
}

func InitializeStaticDatabase() *StaticDatabase {
	start := time.Now()

	db := &StaticDatabase{}
	db.FAA.navaids = parseNavaids()
	db.FAA.airports = parseAirports()
	db.FAA.fixes = parseFixes()
	db.FAA.prd = parsePRD()
	db.callsigns = parseCallsigns()

	lg.Printf("Parsed built-in databases in %v", time.Since(start))

	return db
}

///////////////////////////////////////////////////////////////////////////
// FAA databases

var (
	// https://www.faa.gov/air_traffic/flight_info/aeronav/aero_data/NASR_Subscription_2022-07-14/
	//go:embed resources/NAV_BASE.csv.zst
	navBaseRaw string
	//go:embed resources/APT_BASE.csv.zst
	airportsRaw string
	//go:embed resources/FIX_BASE.csv.zst
	fixesRaw string
	//go:embed resources/callsigns.csv.zst
	callsignsRaw string
	//go:embed resources/virtual-callsigns.csv.zst
	virtualCallsignsRaw string
	//go:embed resources/prefroutes_db.csv.zst
	prdRaw string

	// Via Arash Partow, MIT licensed
	// https://www.partow.net/miscellaneous/airportdatabase/
	//go:embed resources/GlobalAirportDatabase.txt.zst
	globalAirportsRaw string
)

// Utility function for parsing CSV files as strings; it breaks each line
// of the file into fields and calls the provided callback function for
// each one.
func mungeCSV(filename string, raw string, callback func([]string)) {
	r := bytes.NewReader([]byte(raw))
	cr := csv.NewReader(r)
	cr.ReuseRecord = true

	// Skip the first line with the legend
	if _, err := cr.Read(); err != nil {
		lg.Errorf("%s: error parsing CSV file: %s", filename, err)
		return
	}

	for {
		if record, err := cr.Read(); err == io.EOF {
			return
		} else if err != nil {
			lg.Errorf("%s: error parsing CSV file: %s", filename, err)
			return
		} else {
			callback(record)
		}
	}
}

func parseNavaids() map[string]Navaid {
	navaids := make(map[string]Navaid)

	mungeCSV("navaids", decompressZstd(navBaseRaw), func(s []string) {
		n := Navaid{id: s[1], navtype: s[2], name: s[7],
			location: Point2LLFromComponents(s[22:26], s[26:30])}
		if n.id != "" {
			navaids[n.id] = n
		}
	})

	return navaids
}

func parseAirports() map[string]Airport {
	airports := make(map[string]Airport)

	// FAA database
	mungeCSV("airports", decompressZstd(airportsRaw), func(s []string) {
		if elevation, err := strconv.ParseFloat(s[24], 64); err != nil {
			lg.Errorf("%s: error parsing elevation: %s", s[24], err)
		} else {
			loc := Point2LLFromComponents(s[15:19], s[19:23])
			ap := Airport{id: s[98], name: s[12], location: loc, elevation: int(elevation)}
			if ap.id != "" {
				airports[ap.id] = ap
			}
		}
	})

	// Global database; this isn't in CSV, so we need to parse it manually.
	r := bytes.NewReader([]byte(decompressZstd(globalAirportsRaw)))
	scan := bufio.NewScanner(r)
	for scan.Scan() {
		line := scan.Text()
		f := strings.Split(line, ":")
		if len(f) != 16 {
			lg.Errorf("Expected 16 fields, got %d: %s", len(f), line)
		}

		if elevation, err := strconv.ParseFloat(f[13], 64); err != nil {
			lg.Errorf("%s: error parsing elevation: %s", f[13], err)
		} else {
			elevation *= 3.28084 // meters to feet

			ap := Airport{
				id:        f[0],
				name:      f[2],
				location:  Point2LLFromStrings(f[14], f[15]),
				elevation: int(elevation)}
			if ap.id != "" {
				airports[ap.id] = ap
			}
		}
	}

	return airports
}

func parseFixes() map[string]Fix {
	fixes := make(map[string]Fix)

	mungeCSV("fixes", decompressZstd(fixesRaw), func(s []string) {
		f := Fix{
			id:       s[1],
			location: Point2LLFromComponents(s[5:9], s[9:13])}
		if f.id != "" {
			fixes[f.id] = f
		}
	})

	return fixes
}

func parsePRD() map[AirportPair][]PRDEntry {
	prd := make(map[AirportPair][]PRDEntry)

	mungeCSV("prd", decompressZstd(prdRaw), func(s []string) {
		entry := PRDEntry{
			Depart:       s[0],
			Route:        s[1],
			Arrive:       s[2],
			Hours:        [3]string{s[3], s[4], s[5]},
			Type:         s[6],
			Area:         s[7],
			Altitude:     s[8],
			Aircraft:     s[9],
			Direction:    s[10],
			Seq:          s[11],
			DepCenter:    s[12],
			ArriveCenter: s[13]}
		if entry.Depart != "" && entry.Arrive != "" {
			prd[AirportPair{entry.Depart, entry.Arrive}] =
				append(prd[AirportPair{entry.Depart, entry.Arrive}], entry)
		}
	})

	return prd
}

func parseCallsigns() map[string]Callsign {
	callsigns := make(map[string]Callsign)

	addCallsign := func(s []string) {
		fix := func(s string) string { return stopShouting(strings.TrimSpace(s)) }

		cs := Callsign{
			company:   fix(s[0]),
			country:   fix(s[1]),
			telephony: fix(s[2]),
			threeltr:  strings.TrimSpace(s[3])}
		if cs.threeltr != "" && cs.threeltr != "..." {
			callsigns[cs.threeltr] = cs
		}
	}

	mungeCSV("callsigns", decompressZstd(callsignsRaw), addCallsign)
	// Do virtual second since we let them take precedence
	mungeCSV("virtual callsigns", decompressZstd(virtualCallsignsRaw), addCallsign)

	return callsigns
}

///////////////////////////////////////////////////////////////////////////
// The Sector File

func Point2LLFromSct2(ll sct2.LatLong) Point2LL {
	return Point2LL{float32(ll.Longitude), float32(ll.Latitude)}
}

func Point2LLFromLL64(latitude, longitude float64) Point2LL {
	return Point2LL{float32(longitude), float32(latitude)}
}

func (db *StaticDatabase) LoadSectorFile(filename string) error {
	lg.Printf("%s: loading sector file", filename)
	sectorFile, err := parseSectorFile(filename)
	db.sectorFileLoadError = err
	if err != nil {
		return err
	}

	// Copy over some basic stuff from the sector file
	db.defaultAirport = sectorFile.DefaultAirport
	db.defaultCenter = Point2LLFromSct2(sectorFile.Center)
	db.NmPerLatitude = float32(sectorFile.NmPerLatitude)
	db.NmPerLongitude = float32(sectorFile.NmPerLongitude)
	db.MagneticVariation = float32(sectorFile.MagneticVariation)
	db.sectorFileId = sectorFile.Id

	// Clear out everything that is set from the sector file contents in
	// case we're loading a new one.
	db.VORs = make(map[string]Point2LL)
	db.NDBs = make(map[string]Point2LL)
	db.fixes = make(map[string]Point2LL)
	db.airports = make(map[string]Point2LL)
	db.runways = make(map[string][]Runway)
	db.ARTCC = nil
	db.ARTCCLow = nil
	db.ARTCCHigh = nil

	// Also clear out all of the things to draw that are derived from the
	// sector file.
	db.runwayCommandBuffer = CommandBuffer{}
	db.geos = nil
	db.regions = nil
	db.SIDs = nil
	db.STARs = nil
	db.lowAirwayLabels = nil
	db.highAirwayLabels = nil
	db.lowAirwayCommandBuffer = CommandBuffer{}
	db.highAirwayCommandBuffer = CommandBuffer{}
	db.labelColorBufferIndex = ColorBufferIndex{}
	db.labels = nil

	// First initialize various databases from the sector file. Start with
	// named locations--VORs, airports, etc.
	loc := func(ls []sct2.NamedLatLong) map[string]Point2LL {
		m := make(map[string]Point2LL)
		for _, loc := range ls {
			if loc.Name != "" {
				m[loc.Name] = Point2LLFromLL64(loc.Latitude, loc.Longitude)
			}
		}
		return m
	}
	db.VORs = loc(sectorFile.VORs)
	db.NDBs = loc(sectorFile.NDBs)
	db.fixes = loc(sectorFile.Fixes)
	db.airports = loc(sectorFile.Airports)

	// Initialize the runways map, which is from airports to a slice of all
	// of their runways.
	db.runways = make(map[string][]Runway)
	for _, r := range sectorFile.Runways {
		if r.Airport == "" {
			continue
		}
		// Two entries--one for each end of the runway.
		for i := 0; i < 2; i++ {
			db.runways[r.Airport] = append(db.runways[r.Airport],
				Runway{
					number:    r.Number[i],
					heading:   float32(r.Heading[i]),
					threshold: Point2LLFromSct2(r.P[i]),
					end:       Point2LLFromSct2(r.P[i^1])})
		}
	}

	// Now initialize assorted StaticDrawables for things in the sector
	// file.
	setupARTCC := func(sfARTCC []sct2.ARTCC) []StaticDrawable {
		var artccs []StaticDrawable
		for _, artcc := range sfARTCC {
			ld := LinesDrawBuilder{}
			for _, seg := range artcc.Segs {
				if seg.P[0].Latitude != 0 || seg.P[0].Longitude != 0 {
					v0 := Point2LLFromSct2(seg.P[0])
					v1 := Point2LLFromSct2(seg.P[1])
					ld.AddLine(v0, v1)
				}
			}

			sd := StaticDrawable{name: artcc.Name, bounds: ld.Bounds()}
			ld.GenerateCommands(&sd.cb)
			artccs = append(artccs, sd)
		}
		return artccs
	}
	db.ARTCC = setupARTCC(sectorFile.ARTCC)
	db.ARTCCLow = setupARTCC(sectorFile.ARTCCLow)
	db.ARTCCHigh = setupARTCC(sectorFile.ARTCCHigh)

	// Regions are more complicated, both because they are general polygons
	// that we need to triangulate but also because we merge adjacent
	// regions with the same name (as happens often in sector files) into a
	// single StaticDrawable; doing so reduces the number of draw calls
	// issued when rendering, which improves performance.
	//
	// (Note that we generally do not want to merge all regions with the
	// same name into a single StaticDrawable; in that case we might have
	// all of the runways for many airports all together, which would
	// reduce the effectiveness of culling; this way tends to e.g., collect
	// the runways for a single airport, given how sector files usually
	// seem to be organized.)
	currentRegionName := "___UNSET___"
	td := TrianglesDrawBuilder{}
	for i, r := range sectorFile.Regions {
		if len(r.P) == 0 {
			lg.Printf("zero vertices in region \"%s\"?", r.Name)
			continue
		}

		if r.Name != currentRegionName {
			// We've come across a new region name; flush out any accumulated triangles
			// from the previous region
			if len(td.indices) > 0 {
				region := StaticDrawable{name: currentRegionName, bounds: td.Bounds()}
				td.GenerateCommands(&region.cb)
				db.regions = append(db.regions, region)
				td.Reset()
			}
			currentRegionName = r.Name
		}

		// Triangulate
		var poly earcut.Polygon
		for _, p := range r.P {
			v := earcut.Vertex{P: [2]float64{p.Longitude, p.Latitude}}
			poly.Vertices = append(poly.Vertices, v)
		}
		tris := earcut.Triangulate(poly)

		for _, tri := range tris {
			v0 := Point2LL{float32(tri.Vertices[0].P[0]), float32(tri.Vertices[0].P[1])}
			v1 := Point2LL{float32(tri.Vertices[1].P[0]), float32(tri.Vertices[1].P[1])}
			v2 := Point2LL{float32(tri.Vertices[2].P[0]), float32(tri.Vertices[2].P[1])}
			td.AddTriangle(v0, v1, v2)
		}

		// And when we're at the last region, also flush out its
		// StaticDrawable.
		if i+1 == len(sectorFile.Regions) && len(td.indices) > 0 {
			region := StaticDrawable{name: r.Name, bounds: td.Bounds()}
			td.GenerateCommands(&region.cb)
			db.regions = append(db.regions, region)
		}
	}

	// Runway lines
	rld := LinesDrawBuilder{}
	for _, runway := range sectorFile.Runways {
		if runway.P[0].Latitude != 0 || runway.P[0].Longitude != 0 {
			rld.AddLine(Point2LLFromSct2(runway.P[0]), Point2LLFromSct2(runway.P[1]))
		}
	}
	rld.GenerateCommands(&db.runwayCommandBuffer)

	// Labels (e.g., taxiways and runways.)
	db.labelColorBufferIndex = NewColorBufferIndex()
	for _, label := range sectorFile.Labels {
		l := Label{name: label.Name, p: Point2LLFromSct2(label.P)}
		db.labelColorBufferIndex.Add(label.Color)
		db.labels = append(db.labels, l)
	}
	db.lowAirwayCommandBuffer, db.lowAirwayLabels = getAirwayCommandBuffers(sectorFile.LowAirways)
	db.highAirwayCommandBuffer, db.highAirwayLabels = getAirwayCommandBuffers(sectorFile.HighAirways)

	// Various things are represented by colored line segments where their
	// color is either given by a color that is #defined in the sector file
	// or by their general type ("SID"). staticColoredLines handles the
	// details of creating a StaticDrawable for such things, including the
	// ColorBufferIndex initialization so that the actual colors used for
	// drawing them can be changed by the user.
	staticColoredLines := func(name string, cs []sct2.ColoredSegment, defaultColorName string) StaticDrawable {
		ld := ColoredLinesDrawBuilder{}
		colorBufferIndex := NewColorBufferIndex()

		for _, seg := range cs {
			// Ignore (0,0) positions, which are sometimes left in sector
			// files to delineate different sections. They are obviously
			// unhelpful as far as the bounding boxes for culling...
			if seg.P[0].Latitude != 0 || seg.P[0].Longitude != 0 {
				if seg.Color != "" {
					colorBufferIndex.Add(seg.Color)
				} else {
					colorBufferIndex.Add(defaultColorName)
				}
				ld.AddLine(Point2LLFromSct2(seg.P[0]), Point2LLFromSct2(seg.P[1]), RGB{})
			}
		}
		cb := CommandBuffer{}
		start, len := ld.GenerateCommands(&cb)

		return StaticDrawable{
			name:             name,
			cb:               cb,
			rgbSlice:         cb.FloatSlice(start, len),
			bounds:           ld.Bounds(),
			colorBufferIndex: colorBufferIndex}
	}

	for _, sid := range sectorFile.SIDs {
		db.SIDs = append(db.SIDs, staticColoredLines(sid.Name, sid.Segs, "SID"))
	}
	for _, star := range sectorFile.STARs {
		db.STARs = append(db.STARs, staticColoredLines(star.Name, star.Segs, "STAR"))
	}
	for _, geo := range sectorFile.Geo {
		db.geos = append(db.geos, staticColoredLines(geo.Name, geo.Segments, "Geo"))
	}

	// Record all of the names of colors set via #define statements in the
	// sector file so that the use is able to redefine them.
	db.sectorFileColors = make(map[string]RGB)
	for _, color := range sectorFile.Colors {
		db.sectorFileColors[color.Name] = RGB{R: color.R, G: color.G, B: color.B}
	}

	// Various post-load tidying.
	for _, scheme := range globalConfig.ColorSchemes {
		// Add any colors in the sector file that aren't in scopes'
		// color schemes.
		for name, color := range db.sectorFileColors {
			if _, ok := scheme.DefinedColors[name]; !ok {
				c := color
				scheme.DefinedColors[name] = &c
			}
		}
	}

	lg.Printf("%s: finished loading sector file", filename)

	return nil
}

func parseSectorFile(sectorFilename string) (*sct2.SectorFile, error) {
	contents, err := os.ReadFile(sectorFilename)
	if err != nil {
		return nil, err
	}

	type SctResult struct {
		sf  *sct2.SectorFile
		err error
	}
	ch := make(chan SctResult)
	panicStack := ""

	// Parse the sector file in a goroutine so that we can catch any panics, put up
	// a friendly error message, but continue running.
	go func() {
		var err error
		var sf *sct2.SectorFile
		defer func() {
			if perr := recover(); perr != nil {
				panicStack = string(debug.Stack())
				lg.Errorf("Panic stack: %s", panicStack)
				err = fmt.Errorf("sct2.Parse panicked: %v", perr)
			}

			// Use a channel for the result so that we wait for the
			// goroutine to finish.
			if err != nil {
				ch <- SctResult{err: err}
			} else {
				ch <- SctResult{sf: sf}
			}

			close(ch)
		}()

		errorCallback := func(err string) {
			lg.Errorf("%s: error parsing sector file: %s", sectorFilename, err)
		}
		sf, err = sct2.Parse(contents, sectorFilename, errorCallback)
	}()

	r := <-ch

	if panicStack != "" {
		// Have to do this here so that it's in the main thread...
		ShowFatalErrorDialog("Unfortunately an unexpected error has occurred while parsing the sector file:\n" +
			sectorFilename + "\n" +
			"Apologies! Please do file a bug and include the vice.log file for this session\nso that " +
			"this bug can be fixed.")
	}

	return r.sf, r.err
}

func getAirwayCommandBuffers(airways []sct2.Airway) (CommandBuffer, []Label) {
	// Airways are tricky since the sector file will have the same segment
	// multiple times when multiple airways are coincident. We'd like to
	// have a single label for each such segment that includes all of the
	// airways names.
	m := make(map[sct2.Segment][]string)
	for _, a := range airways {
		for _, seg := range a.Segs {
			if seg.P[0].Latitude == 0 && seg.P[0].Longitude == 0 {
				continue
			}

			// canonical order
			if seg.P[0].Latitude > seg.P[1].Latitude {
				seg.P[0], seg.P[1] = seg.P[1], seg.P[0]
			}

			// Has it already been labeled for this segment? (It shouldn't
			// be, but...)
			if labels, ok := m[seg]; ok {
				labeled := false
				for _, l := range labels {
					if l == a.Name {
						labeled = true
						break
					}
				}
				if labeled {
					lg.Errorf("Unexpectedly labeled airway: %s in %+v", a.Name, labels)
				} else {
					m[seg] = append(labels, a.Name)
				}
			} else {
				m[seg] = []string{a.Name}
			}
		}
	}

	// Now get working on the command buffer.
	lines := LinesDrawBuilder{}
	var labels []Label
	for seg, l := range m {
		// Join the airway names with slashes and draw them at the midpoint
		// of each airway segment.
		label := strings.Join(l, "/")
		mid := sct2.LatLong{Latitude: (seg.P[0].Latitude + seg.P[1].Latitude) / 2,
			Longitude: (seg.P[0].Longitude + seg.P[1].Longitude) / 2}
		labels = append(labels, Label{name: label, p: Point2LLFromSct2(mid), color: RGB{}})

		lines.AddLine(Point2LLFromSct2(seg.P[0]), Point2LLFromSct2(seg.P[1]))
	}

	cb := CommandBuffer{}
	lines.GenerateCommands(&cb)

	return cb, labels
}

///////////////////////////////////////////////////////////////////////////
// The Position File

func (db *StaticDatabase) LoadPositionFile(filename string) error {
	lg.Printf("%s: loading position file", filename)

	db.positions, db.positionFileLoadError = parsePositionFile(filename)

	lg.Printf("%s: finished loading position file", filename)

	return db.positionFileLoadError
}

func parsePositionFile(filename string) (map[string][]Position, error) {
	m := make(map[string][]Position)

	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if line == "" || line[0] == ';' {
			continue
		}

		fields := strings.Split(line, ":")
		if len(fields) != 11 {
			lg.Printf("%s: expected 11 fields, got %d: [%+v]", filename, len(fields), fields)
			continue
		}

		var frequency float64
		frequency, err := strconv.ParseFloat(fields[2], 32)
		if err != nil {
			lg.Printf("%s: error parsing frequency: [%+v]", err, fields)
			continue
		}
		// Note: parse as octal!
		var lowSquawk, highSquawk int64
		lowSquawk, err = strconv.ParseInt(fields[9], 8, 32)
		if err != nil {
			// This happens for e.g. entries for neighboring ARTCCs
			lowSquawk = -1
		}
		highSquawk, err = strconv.ParseInt(fields[10], 8, 32)
		if err != nil {
			// This happens for e.g. entries for neighboring ARTCCs
			highSquawk = -1
		}

		id := fields[5] + "_" + fields[6]
		p := Position{
			name:      fields[0],
			callsign:  fields[1],
			frequency: Frequency(int(frequency * 1000)),
			sectorId:  fields[3],
			scope:     fields[4],
			id:        id,
			// ignore fields 7/8
			lowSquawk:  Squawk(lowSquawk),
			highSquawk: Squawk(highSquawk)}

		m[id] = append(m[id], p)
	}
	return m, nil
}

///////////////////////////////////////////////////////////////////////////
// Utility methods

// Locate returns the location of a (static) named thing, if we've heard of it.
func (db *StaticDatabase) Locate(name string) (Point2LL, bool) {
	name = strings.ToUpper(name)
	// We'll start with the sector file and then move on to the FAA
	// database if we don't find it.
	if pos, ok := db.VORs[name]; ok {
		return pos, ok
	} else if pos, ok := db.NDBs[name]; ok {
		return pos, ok
	} else if pos, ok := db.fixes[name]; ok {
		return pos, ok
	} else if pos, ok := db.airports[name]; ok {
		return pos, ok
	} else if n, ok := db.FAA.navaids[name]; ok {
		return n.location, ok
	} else if f, ok := db.FAA.fixes[name]; ok {
		return f.location, ok
	} else if ap, ok := db.FAA.airports[name]; ok {
		return ap.location, ok
	} else {
		return Point2LL{}, false
	}
}

func (db *StaticDatabase) SetColorScheme(cs *ColorScheme) {
	for name, color := range cs.DefinedColors {
		db.NamedColorChanged(name, *color)
	}
	db.NamedColorChanged("Geo", cs.Geo)
	db.NamedColorChanged("SID", cs.SID)
	db.NamedColorChanged("STAR", cs.STAR)
}

// NamedColorChanged should be called when a color in the color scheme has
// been updated; it takes care of updating all of the RGB buffers in the
// assorted rendering command buffers to reflect the change.
func (db *StaticDatabase) NamedColorChanged(name string, rgb RGB) {
	update := func(c ColorBufferIndex, slice []float32, name string, rgb RGB) {
		c.Visit(name, func(i int) {
			idx := 6 * i
			slice[idx] = rgb.R
			slice[idx+1] = rgb.G
			slice[idx+2] = rgb.B
			slice[idx+3] = rgb.R
			slice[idx+4] = rgb.G
			slice[idx+5] = rgb.B
		})
	}

	switch name {
	case "Geo":
		for _, geo := range db.geos {
			update(geo.colorBufferIndex, geo.rgbSlice, "Geo", rgb)
		}

	case "SID":
		for _, sid := range db.SIDs {
			update(sid.colorBufferIndex, sid.rgbSlice, "SID", rgb)
		}

	case "STAR":
		for _, star := range db.STARs {
			update(star.colorBufferIndex, star.rgbSlice, "STAR", rgb)
		}

	default:
		db.labelColorBufferIndex.Visit(name, func(i int) {
			db.labels[i].color = rgb
		})

		for _, geo := range db.geos {
			update(geo.colorBufferIndex, geo.rgbSlice, name, rgb)
		}
		for _, sid := range db.SIDs {
			update(sid.colorBufferIndex, sid.rgbSlice, name, rgb)
		}
		for _, star := range db.STARs {
			update(star.colorBufferIndex, star.rgbSlice, name, rgb)
		}
	}
}