package patch

import (
	"sort"
	"strings"
)

// maxCells bounds the work of the nearest-match search (anchor lines times
// file lines times window size). Beyond it the search is skipped.
const maxCells = 40_000_000

// minSimilarity is the score below which a "nearest match" would only mislead.
const minSimilarity = 0.4

// nearest finds the run of lines in content that looks most like anchor. The
// score of a window is the mean, over the non-blank anchor lines, of the best
// similarity any line of the window has to that anchor line (similarity: Dice
// coefficient of the byte bigrams, whitespace collapsed). It returns nil when
// nothing is similar enough or the file is too big to search.
func nearest(content, anchor string) *Nearest {
	cl := strings.Split(content, "\n")
	if len(cl) > 1 && cl[len(cl)-1] == "" {
		cl = cl[:len(cl)-1]
	}
	al := strings.Split(anchor, "\n")
	if len(al) > 1 && al[len(al)-1] == "" {
		al = al[:len(al)-1]
	}
	k := len(al)
	if k > len(cl) {
		k = len(cl)
	}
	var scored []int // indexes of non-blank anchor lines
	for i, l := range al {
		if strings.TrimSpace(l) != "" {
			scored = append(scored, i)
		}
	}
	m := len(scored)
	if m == 0 || k == 0 || m*len(cl)*k > maxCells {
		return nil
	}

	cb := make([][]uint16, len(cl))
	cn := make([]string, len(cl))
	for j, l := range cl {
		cn[j] = collapse(l)
		cb[j] = bigrams(cn[j])
	}
	sim := make([][]float32, m)
	for i, ai := range scored {
		an := collapse(al[ai])
		ab := bigrams(an)
		row := make([]float32, len(cl))
		for j := range cl {
			row[j] = dice(ab, cb[j], an == cn[j])
		}
		sim[i] = row
	}

	best, bestAt := -1.0, 0
	for w := 0; w+k <= len(cl); w++ {
		var sum float64
		for i := range scored {
			var mx float32
			for j := w; j < w+k; j++ {
				if sim[i][j] > mx {
					mx = sim[i][j]
				}
			}
			sum += float64(mx)
		}
		if score := sum / float64(m); score > best {
			best, bestAt = score, w
		}
	}
	if best < minSimilarity {
		return nil
	}
	lines := cl[bestAt : bestAt+k]
	if len(lines) > 30 {
		lines = lines[:30]
	}
	return &Nearest{
		StartLine:  bestAt + 1,
		EndLine:    bestAt + k,
		Similarity: float64(int(best*100+0.5)) / 100,
		Lines:      append([]string(nil), lines...),
	}
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// bigrams returns the sorted byte bigrams of s.
func bigrams(s string) []uint16 {
	if len(s) < 2 {
		return nil
	}
	out := make([]uint16, 0, len(s)-1)
	for i := 0; i+1 < len(s); i++ {
		out = append(out, uint16(s[i])<<8|uint16(s[i+1]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// dice is the Dice coefficient of two sorted bigram multisets; for lines too
// short to have bigrams it falls back to equality.
func dice(a, b []uint16, equal bool) float32 {
	if len(a) == 0 || len(b) == 0 {
		if equal {
			return 1
		}
		return 0
	}
	i, j, common := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			common++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return float32(2*common) / float32(len(a)+len(b))
}

// hints explains cheap-to-check reasons why anchor does not match text.
func hints(text, anchor, ending string) []string {
	var out []string
	if ending == "mixed" {
		out = append(out, "the file mixes LF and CRLF line endings; multi-line anchors are matched byte for byte")
	}
	if strings.Contains(strings.ToLower(text), strings.ToLower(anchor)) {
		out = append(out, "the anchor matches when upper and lower case are ignored")
	}
	if ca := collapse(anchor); ca != "" && strings.Contains(collapse(text), ca) {
		out = append(out, "the anchor matches when runs of whitespace are ignored: check indentation, tabs against spaces, and trailing spaces")
	}
	return out
}
