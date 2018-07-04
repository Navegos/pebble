package ptable

import (
	"fmt"
	"testing"

	"github.com/petermattis/pebble/cache"
	"github.com/petermattis/pebble/db"
	"github.com/petermattis/pebble/storage"
)

type testEnv []ColumnDef

func (e testEnv) Encode(row RowReader, buf []byte) (key, value []byte) {
	for i := range e {
		switch e[i].Type {
		case ColumnTypeInt64:
			key = append(key, []byte(fmt.Sprintf("%08d", row.Int64(i)))...)
			break
		default:
			panic("not reached")
		}
	}

	return key, nil
}

func (e testEnv) Decode(key, value, buf []byte, writer RowWriter) {
}

func newEnv(schema ...ColumnDef) *Env {
	t := testEnv(schema)
	return &Env{
		Schema: schema,
		Encode: t.Encode,
		Decode: t.Decode,
	}
}

type testRow []interface{}

func makeRow(cols ...interface{}) testRow {
	return testRow(cols)
}

func (r testRow) Null(col int) bool {
	return r[col] == nil
}

func (r testRow) Bool(col int) bool {
	return r[col].(bool)
}

func (r testRow) Int8(col int) int8 {
	return r[col].(int8)
}

func (r testRow) Int16(col int) int16 {
	return r[col].(int16)
}

func (r testRow) Int32(col int) int32 {
	return r[col].(int32)
}

func (r testRow) Int64(col int) int64 {
	return r[col].(int64)
}

func (r testRow) Float32(col int) float32 {
	return r[col].(float32)
}

func (r testRow) Float64(col int) float64 {
	return r[col].(float64)
}

func (r testRow) Bytes(col int) []byte {
	switch t := r[col].(type) {
	case []byte:
		return t
	case string:
		return []byte(t)
	default:
		panic("not reached")
	}
}

func TestTable(t *testing.T) {
	const count int64 = 1000
	mem := storage.NewMem()
	env := newEnv(ColumnDef{Type: ColumnTypeInt64})

	{
		f, err := mem.Create("test")
		if err != nil {
			t.Fatal(err)
		}
		w := NewWriter(f, env, &db.Options{BlockSize: 100})
		for i := int64(0); i < 1000; i++ {
			if err := w.AddRow(makeRow(i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}

	{
		f, err := mem.Open("test")
		if err != nil {
			t.Fatal(err)
		}
		r := NewReader(f, 0, nil)
		iter := r.NewIter()
		var j int64
		for iter.First(); iter.Valid(); iter.Next() {
			col := iter.Block().Column(0).Int64()
			for _, i := range col {
				if j != i {
					t.Fatalf("expected %d, but found %d", j, i)
				}
				j++
			}
		}
		if count != j {
			t.Fatalf("expected %d, but found %d", count, j)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func buildBenchmarkTable(b *testing.B, blockSize int) *Reader {
	mem := storage.NewMem()
	f0, err := mem.Create("bench")
	if err != nil {
		b.Fatal(err)
	}
	defer f0.Close()

	env := newEnv(ColumnDef{Type: ColumnTypeInt64})
	w := NewWriter(f0, env, &db.Options{BlockSize: blockSize})
	for i := int64(0); i < 1e6; i++ {
		w.AddRow(makeRow(i))
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}

	// Re-open that filename for reading.
	f1, err := mem.Open("bench")
	if err != nil {
		b.Fatal(err)
	}
	return NewReader(f1, 0, &db.Options{
		Cache: cache.NewBlockCache(128 << 20),
	})
}

func BenchmarkTableIterNext(b *testing.B) {
	const blockSize = 32 << 10

	r := buildBenchmarkTable(b, blockSize)
	it := r.NewIter()

	b.ResetTimer()
	var sum int64
	for i, k := 0, 0; i < b.N; i += k {
		if !it.Valid() {
			it.First()
		}

		vals := it.Block().Column(0).Int64()
		k = len(vals)
		if k > b.N-i {
			k = b.N - i
		}
		for j := 0; j < k; j++ {
			sum += vals[j]
		}

		it.Next()
	}
}

func BenchmarkTableIterPrev(b *testing.B) {
	const blockSize = 32 << 10

	r := buildBenchmarkTable(b, blockSize)
	it := r.NewIter()

	b.ResetTimer()
	var sum int64
	for i, k := 0, 0; i < b.N; i += k {
		if !it.Valid() {
			it.Last()
		}

		vals := it.Block().Column(0).Int64()
		k = len(vals)
		if k > b.N-i {
			k = b.N - i
		}
		for j, e := len(vals)-1, len(vals)-k; j >= e; j-- {
			sum += vals[j]
		}

		it.Prev()
	}
}
