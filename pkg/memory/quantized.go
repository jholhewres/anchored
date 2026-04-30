package memory

import (
	"encoding/binary"
	"fmt"
	"math"
)

type QuantizedEmbedding struct {
	Data   []uint8
	Scale  float32
	MinVal float32
	Dims   int
}

func QuantizeFloat32(vec []float32) QuantizedEmbedding {
	if len(vec) == 0 {
		return QuantizedEmbedding{Dims: 0}
	}

	minVal := vec[0]
	maxVal := vec[0]
	for _, v := range vec[1:] {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}

	span := maxVal - minVal
	var scale float32
	if span > 0 {
		scale = span / 255.0
	}

	data := make([]uint8, len(vec))
	if span > 0 {
		invSpan := 255.0 / float64(span)
		for i, v := range vec {
			data[i] = uint8(math.Round(float64(v-minVal) * invSpan))
		}
	}

	return QuantizedEmbedding{
		Data:   data,
		Scale:  scale,
		MinVal: minVal,
		Dims:   len(vec),
	}
}

func (q QuantizedEmbedding) Dequantize() []float32 {
	out := make([]float32, q.Dims)
	for i := 0; i < q.Dims; i++ {
		out[i] = float32(q.Data[i])*q.Scale + q.MinVal
	}
	return out
}

func (q QuantizedEmbedding) DotProduct(query []float32) float64 {
	if len(query) != q.Dims {
		return 0
	}

	var sumDQ float64
	var sumQ float64

	for i := 0; i < q.Dims; i++ {
		qi := float64(query[i])
		sumDQ += float64(q.Data[i]) * qi
		sumQ += qi
	}

	return float64(q.Scale)*sumDQ + float64(q.MinVal)*sumQ
}

func (q QuantizedEmbedding) CosineSimilarity(query []float32, queryNorm float64) float64 {
	if len(query) != q.Dims || queryNorm == 0 {
		return 0
	}

	dot := q.DotProduct(query)

	var sumD2 float64
	var sumD float64
	for i := 0; i < q.Dims; i++ {
		d := float64(q.Data[i])
		sumD2 += d * d
		sumD += d
	}

	s := float64(q.Scale)
	m := float64(q.MinVal)
	normSq := s*s*sumD2 + 2*s*m*sumD + float64(q.Dims)*m*m

	if normSq <= 0 {
		return 0
	}

	return dot / (math.Sqrt(normSq) * queryNorm)
}

const binaryHeaderSize = 12

func (q QuantizedEmbedding) MarshalBinary() ([]byte, error) {
	buf := make([]byte, binaryHeaderSize+q.Dims)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(q.Dims))
	binary.LittleEndian.PutUint32(buf[4:8], math.Float32bits(q.Scale))
	binary.LittleEndian.PutUint32(buf[8:12], math.Float32bits(q.MinVal))
	copy(buf[binaryHeaderSize:], q.Data)
	return buf, nil
}

func (q *QuantizedEmbedding) UnmarshalBinary(data []byte) error {
	if len(data) < binaryHeaderSize {
		return fmt.Errorf("quantized embedding: binary too short (%d bytes)", len(data))
	}

	q.Dims = int(binary.LittleEndian.Uint32(data[0:4]))
	const maxEmbeddingDims = 65536
	if q.Dims > maxEmbeddingDims {
		return fmt.Errorf("quantized embedding: dims %d exceeds maximum %d", q.Dims, maxEmbeddingDims)
	}
	q.Scale = math.Float32frombits(binary.LittleEndian.Uint32(data[4:8]))
	q.MinVal = math.Float32frombits(binary.LittleEndian.Uint32(data[8:12]))

	expected := binaryHeaderSize + q.Dims
	if len(data) != expected {
		return fmt.Errorf("quantized embedding: expected %d bytes, got %d", expected, len(data))
	}

	q.Data = make([]uint8, q.Dims)
	copy(q.Data, data[binaryHeaderSize:])
	return nil
}

func VectorNorm(v []float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}
