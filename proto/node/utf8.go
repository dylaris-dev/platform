package node

import (
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// ValidUTF8 replaces invalid UTF-8 in every string field of m, in place.
// proto3 refuses to marshal such a string, and grpc-go ends the whole
// NodeConnect stream on a marshal error, so one tenant-chosen file name, path
// or header cut every transfer, console and tab of every tenant on a node.
// Both sides run it at their one send choke point.
//
// Hand-written, not generated: protoc leaves this file alone.
//
// ponytail: no map fields in node.proto; walk them here if one is added.
func ValidUTF8(m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				if fd.Kind() == protoreflect.StringKind {
					if s := l.Get(i).String(); !utf8.ValidString(s) {
						l.Set(i, protoreflect.ValueOfString(strings.ToValidUTF8(s, "�")))
					}
				} else if fd.Message() != nil {
					ValidUTF8(l.Get(i).Message())
				}
			}
		case fd.Kind() == protoreflect.StringKind:
			if s := v.String(); !utf8.ValidString(s) {
				m.Set(fd, protoreflect.ValueOfString(strings.ToValidUTF8(s, "�")))
			}
		case fd.Message() != nil:
			ValidUTF8(v.Message())
		}
		return true
	})
}
