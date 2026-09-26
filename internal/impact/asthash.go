package impact

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"hash"
	"hash/fnv"
	"reflect"
	"sync"
)

// astHash hashes the syntax of node, ignoring comments, source positions and
// the parser's deprecated object links. It covers the same fields as
// ast.Fprint would, but writes compact binary data instead of formatted text,
// which made hashing several times faster. It only reads the tree, so it is
// safe to call concurrently on shared syntax.
func astHash(node ast.Node) (string, error) {
	encoder := astEncoder{hash: sha256.New()}
	if err := encoder.value(reflect.ValueOf(node)); err != nil {
		return "", err
	}
	encoder.flush()
	return hex.EncodeToString(encoder.hash.Sum(nil)), nil
}

type astEncoder struct {
	hash   hash.Hash
	buffer []byte
}

func (e *astEncoder) value(value reflect.Value) error {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			e.buffer = append(e.buffer, 0)
			return nil
		}
		e.buffer = append(e.buffer, 1)
		return e.value(value.Elem())
	case reflect.Pointer:
		if value.IsNil() {
			e.buffer = append(e.buffer, 0)
			return nil
		}
		// The type distinguishes nodes with the same shape, such as
		// *ast.StarExpr and *ast.ParenExpr.
		e.buffer = append(e.buffer, 1)
		e.buffer = binary.LittleEndian.AppendUint32(e.buffer, typeTag(value.Type()))
		return e.value(value.Elem())
	case reflect.Struct:
		for _, field := range astFields(value.Type()) {
			fieldValue := value.Field(field.index)
			if field.presence {
				e.buffer = append(e.buffer, boolByte(fieldValue.Int() != int64(token.NoPos)))
				continue
			}
			if err := e.value(fieldValue); err != nil {
				return err
			}
		}
	case reflect.Slice:
		e.buffer = binary.AppendVarint(e.buffer, int64(value.Len()))
		for index := range value.Len() {
			if err := e.value(value.Index(index)); err != nil {
				return err
			}
		}
	case reflect.String:
		e.buffer = binary.AppendVarint(e.buffer, int64(value.Len()))
		e.buffer = append(e.buffer, value.String()...)
	case reflect.Bool:
		e.buffer = append(e.buffer, boolByte(value.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.buffer = binary.AppendVarint(e.buffer, value.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		e.buffer = binary.AppendUvarint(e.buffer, value.Uint())
	default:
		return fmt.Errorf("hash syntax: unsupported %s", value.Type())
	}
	if len(e.buffer) >= 32<<10 {
		e.flush()
	}
	return nil
}

func (e *astEncoder) flush() {
	_, _ = e.hash.Write(e.buffer)
	e.buffer = e.buffer[:0]
}

func boolByte(value bool) byte {
	if value {
		return 1
	}
	return 0
}

type astField struct {
	index int
	// presence records only whether a position is set: f(xs...) and
	// type A = B differ from f(xs) and type A B by one position field.
	presence bool
}

var (
	positionType  = reflect.TypeFor[token.Pos]()
	astFieldCache sync.Map // reflect.Type -> []astField
	typeTagCache  sync.Map // reflect.Type -> uint32
)

func astFields(typ reflect.Type) []astField {
	if cached, ok := astFieldCache.Load(typ); ok {
		return cached.([]astField)
	}
	var fields []astField
	for index := range typ.NumField() {
		field := typ.Field(index)
		switch field.Name {
		case "Doc", "Comment", "Comments", "Obj", "Scope", "Unresolved":
			continue
		}
		if field.Type == positionType {
			if typ == reflect.TypeFor[ast.CallExpr]() && field.Name == "Ellipsis" ||
				typ == reflect.TypeFor[ast.TypeSpec]() && field.Name == "Assign" {
				fields = append(fields, astField{index: index, presence: true})
			}
			continue
		}
		fields = append(fields, astField{index: index})
	}
	astFieldCache.Store(typ, fields)
	return fields
}

// typeTag is derived from the type name, so it is stable across processes
// and cached hashes stay comparable.
func typeTag(typ reflect.Type) uint32 {
	if cached, ok := typeTagCache.Load(typ); ok {
		return cached.(uint32)
	}
	tag := fnv.New32a()
	_, _ = tag.Write([]byte(typ.String()))
	value := tag.Sum32()
	typeTagCache.Store(typ, value)
	return value
}
