package chunk

func Bounds(g *RawGeometry) ([4]float64, bool) {
	parsed, err := parseGeometry(g)
	if err != nil {
		return [4]float64{}, false
	}
	return parsed.Bound()
}

func FeatureBounds(f *InputFeature) ([4]float64, bool) {
	if f.Parsed != nil {
		return f.Parsed.Bound()
	}
	return Bounds(&f.Geometry)
}

func (p *ParsedGeometry) Bound() ([4]float64, bool) {
	var b [4]float64
	seen := false
	expandBounds(p, &b, &seen)
	return b, seen
}

func expandBounds(p *ParsedGeometry, b *[4]float64, seen *bool) {
	for i := 0; i+1 < len(p.xy); i += 2 {
		x, y := p.xy[i], p.xy[i+1]
		if !*seen {
			b[0], b[1], b[2], b[3] = x, y, x, y
			*seen = true
			continue
		}
		if x < b[0] {
			b[0] = x
		}
		if y < b[1] {
			b[1] = y
		}
		if x > b[2] {
			b[2] = x
		}
		if y > b[3] {
			b[3] = y
		}
	}
	for i := range p.parts {
		expandBounds(&p.parts[i], b, seen)
	}
}
