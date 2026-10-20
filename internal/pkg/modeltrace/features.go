package modeltrace

import "math"

func dot(a, b []float64) float64 {
	s := 0.
	for i, v := range a {
		s += v * b[i]
	}
	return s
}
func standardize(v []float64) []float64 {
	sum := 0.
	for _, x := range v {
		sum += x
	}
	mean := sum / float64(len(v))
	variance := 0.
	for _, x := range v {
		d := x - mean
		variance += d * d
	}
	scale := math.Max(math.Sqrt(variance/float64(len(v))), 1e-12)
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = (x - mean) / scale
	}
	return out
}
func normalized(v []float64) []float64 {
	scale := math.Max(math.Sqrt(dot(v, v)), 1e-12)
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = x / scale
	}
	return out
}
func subtract(v []float64, basis [][]float64) []float64 {
	out := append([]float64(nil), v...)
	for _, b := range basis {
		p := dot(out, b)
		for i := range out {
			out[i] -= p * b[i]
		}
	}
	return out
}
func counts(numbers []int) []float64 {
	c := make([]float64, 355)
	for _, n := range numbers {
		c[n-1]++
	}
	return c
}
func hellinger(c []float64) []float64 {
	total := 0.
	for _, v := range c {
		total += v
	}
	total += .5 * 355
	out := make([]float64, 355)
	for i, v := range c {
		out[i] = math.Sqrt((v + .5) / total)
	}
	return out
}
func ordered(numbers []int) []float64 {
	out := make([]float64, 0, 74)
	base, rest, start := len(numbers)/4, len(numbers)%4, 0
	for b := 0; b < 4; b++ {
		size := base
		if b < rest {
			size++
		}
		bins := make([]float64, 16)
		for i := range bins {
			bins[i] = .5
		}
		for _, v := range numbers[start : start+size] {
			idx := min(15, int(math.Floor((float64(v-1)/355)*16)))
			bins[idx]++
		}
		total := 0.
		for _, v := range bins {
			total += v
		}
		for _, v := range bins {
			out = append(out, math.Sqrt(v/total))
		}
		start += size
	}
	last := make([]float64, 10)
	for i := range last {
		last[i] = .5
	}
	for _, v := range numbers {
		last[v%10]++
	}
	total := 0.
	for _, v := range last {
		total += v
	}
	for _, v := range last {
		out = append(out, math.Sqrt(v/total))
	}
	return out
}
func transform(f []float64, a artifactDefinition) []float64 {
	out := make([]float64, len(f))
	for i, v := range f {
		out[i] = (v - a.Mean[i]) / a.Scale[i]
	}
	return out
}
func centroidScores(v []float64, centroids [][]float64) []float64 {
	out := make([]float64, len(centroids))
	for i, c := range centroids {
		out[i] = dot(v, c)
	}
	return out
}
func scoreNumbers(numbers []int, b *bank) []float64 {
	a := b.Robust.Hellinger
	v := normalized(subtract(transform(hellinger(counts(numbers)), a), a.Basis))
	marginal := standardize(centroidScores(v, a.Centroids))
	o := b.Robust.Ordered
	if o == nil || o.Weight == 0 {
		return marginal
	}
	f := transform(ordered(numbers), *o)
	unit := normalized(f)
	template := make([]float64, len(b.Models))
	for i := range template {
		template[i] = math.Inf(-1)
	}
	for _, env := range o.Environments {
		scores := centroidScores(unit, env)
		for i, v := range scores {
			template[i] = math.Max(template[i], v)
		}
	}
	template = standardize(template)
	nuisance := standardize(centroidScores(normalized(subtract(f, o.Basis)), o.Centroids))
	mix := make([]float64, len(template))
	for i := range mix {
		mix[i] = .5*template[i] + .5*nuisance[i]
	}
	mix = standardize(mix)
	for i := range marginal {
		marginal[i] = (1-o.Weight)*marginal[i] + o.Weight*mix[i]
	}
	return marginal
}
func similarity(left, right []float64) float64 {
	lt, rt := 0., 0.
	for _, v := range left {
		lt += v
	}
	for _, v := range right {
		rt += v
	}
	rt += .5 * 355
	p, q, mid := make([]float64, 355), make([]float64, 355), make([]float64, 355)
	for i := range p {
		p[i] = left[i] / lt
		q[i] = (right[i] + .5) / rt
		mid[i] = (p[i] + q[i]) / 2
	}
	div := func(v []float64) float64 {
		sum := 0.
		for i, x := range v {
			if x != 0 {
				sum += x * math.Log(x/mid[i])
			}
		}
		return sum
	}
	return 1 - math.Sqrt(((div(p)+div(q))/2)/math.Log(2))
}
