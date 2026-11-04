package collect

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
)

// rawStep is one entry of a provenance's llbDefinition.
type rawStep struct {
	id     string
	op     jsontext.Value
	inputs []string
}

// rawProvenance holds the parts of one SLSA v1 provenance that Ichnos reads.
// defined is false when the provenance has no buildDefinition, which every
// SLSA v1 provenance has, and config is false when it has no build
// configuration.
type rawProvenance struct {
	defined   bool
	config    bool
	platform  string
	steps     []rawStep
	digests   map[string]string
	locations map[string][]location
	sources   [][]byte
}

// readBuildDefinition and readRunDetails read the two top-level fields of
// a provenance as a stream. They decode only the fields Ichnos uses, check
// every list against its limit before they keep another element, and skip
// everything else without decoding it, so their memory grows with what they
// keep, never with what they skip. A string they keep must be valid UTF-8;
// a field they skip need not be.
func (b *jsonBlobReader) readBuildDefinition(j *jsonReader, p *rawProvenance) error {
	p.defined = true
	return j.fields([]string{"internalParameters"}, func(int) error {
		return j.fields([]string{"buildConfig", "targetPlatform"}, func(field int) error {
			if field == 1 {
				var err error
				p.platform, err = j.str()
				return err
			}
			if j.peek() == jsontext.KindNull {
				return j.skip()
			}
			p.config = true
			return j.fields([]string{"llbDefinition", "digestMapping"}, func(field int) error {
				if field == 0 {
					return j.elements(func() error { return b.readStep(j, p) })
				}
				return j.entries(func(digest []byte) error { return b.readMapping(j, p, string(digest)) })
			})
		})
	})
}

func (b *jsonBlobReader) readRunDetails(j *jsonReader, p *rawProvenance) error {
	return j.fields([]string{"metadata"}, func(int) error {
		return j.fields([]string{"buildkit_metadata"}, func(int) error {
			return j.fields([]string{"source"}, func(int) error {
				return j.fields([]string{"locations", "infos"}, func(field int) error {
					if field == 0 {
						return j.entries(func(id []byte) error { return b.readLocations(j, p, string(id)) })
					}
					return j.elements(func() error { return b.readSource(j, p) })
				})
			})
		})
	})
}

func (b *jsonBlobReader) readStep(j *jsonReader, p *rawProvenance) error {
	if b.steps == b.lim.steps {
		return &LimitError{What: "steps", Limit: int64(b.lim.steps)}
	}
	b.steps++
	var st rawStep
	err := j.fields([]string{"id", "op", "inputs"}, func(field int) error {
		switch field {
		case 0:
			var err error
			st.id, err = j.str()
			return err
		case 1:
			raw, err := j.readValue()
			if err != nil {
				return err
			}
			// raw is valid only until the next read, so the step keeps a copy.
			if st.op, err = stepDefinition(raw); st.op != nil {
				st.op = bytes.Clone(st.op)
			}
			return err
		default:
			return j.elements(func() error {
				if b.inputs == b.lim.inputs {
					return &LimitError{What: "step inputs", Limit: int64(b.lim.inputs)}
				}
				b.inputs++
				in, err := j.str()
				st.inputs = append(st.inputs, in)
				return err
			})
		}
	})
	p.steps = append(p.steps, st)
	return err
}

// readMapping reads one entry of digestMapping, which has one entry per
// step and so shares the step limit.
func (b *jsonBlobReader) readMapping(j *jsonReader, p *rawProvenance, digest string) error {
	if b.mappings == b.lim.steps {
		return &LimitError{What: "steps", Limit: int64(b.lim.steps)}
	}
	b.mappings++
	id, err := j.str()
	if err != nil {
		return err
	}
	if _, dup := p.digests[digest]; dup {
		return malformed("provenance maps one digest twice")
	}
	if p.digests == nil {
		p.digests = map[string]string{}
	}
	p.digests[digest] = id
	return nil
}

func (b *jsonBlobReader) readLocations(j *jsonReader, p *rawProvenance, id string) error {
	return j.fields([]string{"locations"}, func(int) error {
		return j.elements(func() error {
			source := 0
			var ranges []lineRange
			err := j.fields([]string{"sourceIndex", "ranges"}, func(field int) error {
				if field == 0 {
					var err error
					source, err = j.integer()
					return err
				}
				return j.elements(func() error {
					if b.ranges == b.lim.ranges {
						return &LimitError{What: "source ranges", Limit: int64(b.lim.ranges)}
					}
					b.ranges++
					var r lineRange
					err := j.fields([]string{"start", "end"}, func(field int) error {
						return j.fields([]string{"line"}, func(int) error {
							var err error
							if field == 0 {
								r.start, err = j.integer()
							} else {
								r.end, err = j.integer()
							}
							return err
						})
					})
					ranges = append(ranges, r)
					return err
				})
			})
			if err != nil {
				return err
			}
			if len(ranges) > 0 && p.locations == nil {
				p.locations = map[string][]location{}
			}
			for _, r := range ranges {
				p.locations[id] = append(p.locations[id], location{source: source, lines: r})
			}
			return nil
		})
	})
}

func (b *jsonBlobReader) readSource(j *jsonReader, p *rawProvenance) error {
	if b.sources == b.lim.sources {
		return &LimitError{What: "source files", Limit: int64(b.lim.sources)}
	}
	b.sources++
	var data []byte
	err := j.fields([]string{"data"}, func(int) error {
		if j.peek() == jsontext.KindNull {
			return j.skip()
		}
		_, enc, err := j.readBytes()
		if err != nil {
			return err
		}
		data = make([]byte, base64.StdEncoding.DecodedLen(len(enc)))
		n, err := base64.StdEncoding.Decode(data, enc)
		if err != nil {
			return malformed("provenance has a source that is not valid base64")
		}
		data = data[:n]
		return nil
	})
	p.sources = append(p.sources, data)
	return err
}
