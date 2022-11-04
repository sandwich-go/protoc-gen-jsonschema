package converter

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/protoc-gen-go/descriptor"
	"github.com/khadgarmage/jsonschema"
	"github.com/xeipuuv/gojsonschema"
)

var (
	globalPkg = &ProtoPackage{
		name:     "",
		parent:   nil,
		children: make(map[string]*ProtoPackage),
		types:    make(map[string]*descriptor.DescriptorProto),
	}
)

func (c *Converter) registerType(pkgName *string, msg *descriptor.DescriptorProto) {
	pkg := globalPkg
	if pkgName != nil {
		for _, node := range strings.Split(*pkgName, ".") {
			if pkg == globalPkg && node == "" {
				// Skips leading "."
				continue
			}
			child, ok := pkg.children[node]
			if !ok {
				child = &ProtoPackage{
					name:     pkg.name + "." + node,
					parent:   pkg,
					children: make(map[string]*ProtoPackage),
					types:    make(map[string]*descriptor.DescriptorProto),
				}
				pkg.children[node] = child
			}
			pkg = child
		}
	}
	pkg.types[msg.GetName()] = msg
}

func (c *Converter) relativelyLookupNestedType(desc *descriptor.DescriptorProto, name string) (*descriptor.DescriptorProto, bool) {
	components := strings.Split(name, ".")
componentLoop:
	for _, component := range components {
		for _, nested := range desc.GetNestedType() {
			if nested.GetName() == component {
				desc = nested
				continue componentLoop
			}
		}
		c.logger.WithField("component", component).WithField("description", desc.GetName()).Info("no such nested message")
		return nil, false
	}
	return desc, true
}

// Convert a proto "field" (essentially a type-switch with some recursion):
func (c *Converter) convertField(curPkg *ProtoPackage, desc *descriptor.FieldDescriptorProto,
	msg *descriptor.DescriptorProto, outerEnums []*descriptor.EnumDescriptorProto) (*jsonschema.Type, error) {

	// Prepare a new jsonschema.Type for our eventual return value:
	jsonSchemaType := &jsonschema.Type{
		Properties: make(map[string]*jsonschema.Type),
	}

	// Switch the types, and pick a JSONSchema equivalent:
	switch desc.GetType() {
	case descriptor.FieldDescriptorProto_TYPE_DOUBLE,
		descriptor.FieldDescriptorProto_TYPE_FLOAT:
			jsonSchemaType.Type = gojsonschema.TYPE_NUMBER

	case descriptor.FieldDescriptorProto_TYPE_INT32,
		descriptor.FieldDescriptorProto_TYPE_UINT32,
		descriptor.FieldDescriptorProto_TYPE_FIXED32,
		descriptor.FieldDescriptorProto_TYPE_SFIXED32,
		descriptor.FieldDescriptorProto_TYPE_SINT32:
			jsonSchemaType.Type = gojsonschema.TYPE_INTEGER

	case descriptor.FieldDescriptorProto_TYPE_INT64,
		descriptor.FieldDescriptorProto_TYPE_UINT64,
		descriptor.FieldDescriptorProto_TYPE_FIXED64,
		descriptor.FieldDescriptorProto_TYPE_SFIXED64,
		descriptor.FieldDescriptorProto_TYPE_SINT64:
			jsonSchemaType.Type = gojsonschema.TYPE_INTEGER

	case descriptor.FieldDescriptorProto_TYPE_STRING,
		descriptor.FieldDescriptorProto_TYPE_BYTES:
			jsonSchemaType.Type = gojsonschema.TYPE_STRING

	case descriptor.FieldDescriptorProto_TYPE_ENUM:
		jsonSchemaType.Type = gojsonschema.TYPE_INTEGER

		// Go through all the enums we have, see if we can match any to this field by name:
		enums := append(msg.GetEnumType(), outerEnums...)
		for _, enumDescriptor := range enums {
			// Each one has several values:
			for _, enumValue := range enumDescriptor.Value {

				// Figure out the entire name of this field:
				// If we find ENUM values for this field then put them into the JSONSchema list of allowed ENUM values:
				if strings.HasSuffix(desc.GetTypeName(), *enumDescriptor.Name) {
					jsonSchemaType.Enum = append(jsonSchemaType.Enum, enumValue.Number)
					if src := c.sourceInfo.GetEnumValue(enumValue); src != nil {
						if s := getEnumComment(src); len(s) > 0 {
							jsonSchemaType.OptionLabels = append(jsonSchemaType.OptionLabels, s)
							continue
						}
					}
					jsonSchemaType.OptionLabels = append(jsonSchemaType.OptionLabels, enumValue.Number)
				}
			}
		}

	case descriptor.FieldDescriptorProto_TYPE_BOOL:
		jsonSchemaType.Type = gojsonschema.TYPE_BOOLEAN

	case descriptor.FieldDescriptorProto_TYPE_GROUP,
		descriptor.FieldDescriptorProto_TYPE_MESSAGE:
		jsonSchemaType.Type = gojsonschema.TYPE_OBJECT
		if desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_OPTIONAL {
			jsonSchemaType.AdditionalProperties = []byte("true")
		}
		if desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_REQUIRED {
			jsonSchemaType.AdditionalProperties = []byte("false")
		}

	default:
		return nil, fmt.Errorf("unrecognized field type: %s", desc.GetType().String())
	}
	// Recurse array of primitive types:
	if desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_REPEATED && jsonSchemaType.Type != gojsonschema.TYPE_OBJECT {
		jsonSchemaType.Items = &jsonschema.Type{}

		if len(jsonSchemaType.Enum) > 0 {
			jsonSchemaType.Items.Enum = jsonSchemaType.Enum
			jsonSchemaType.Enum = nil
			jsonSchemaType.Items.OneOf = nil
		} else {
			jsonSchemaType.Items.Type = jsonSchemaType.Type
			jsonSchemaType.Items.OneOf = jsonSchemaType.OneOf
		}

		if c.AllowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Type{
				{Type: gojsonschema.TYPE_NULL},
				{Type: gojsonschema.TYPE_ARRAY},
			}
		} else {
			jsonSchemaType.Type = gojsonschema.TYPE_ARRAY
			jsonSchemaType.OneOf = []*jsonschema.Type{}
		}
		return jsonSchemaType, nil
	}

	// Recurse nested objects / arrays of objects (if necessary):
	if jsonSchemaType.Type == gojsonschema.TYPE_OBJECT {
		recordType, ok := c.lookupType(curPkg, desc.GetTypeName())
		if !ok {
			return nil, fmt.Errorf("no such message type named %s", desc.GetTypeName())
		}

		// Recurse the recordType:
		recursedJSONSchemaType, err := c.convertMessageType(curPkg, recordType, outerEnums)
		if err != nil {
			return nil, err
		}
		// Maps, arrays, and objects are structured in different ways:
		if (recordType.Options.GetMapEntry() || desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_REPEATED) {
			c.logger.
				WithField("field_name", recordType.GetName()).
				WithField("msg_name", *msg.Name)
			jsonSchemaType.Items = &recursedJSONSchemaType
			jsonSchemaType.Type = gojsonschema.TYPE_ARRAY
		} else {
			jsonSchemaType.Properties = recursedJSONSchemaType.Properties
		}

		// Optionally allow NULL values:

	}

	return jsonSchemaType, nil
}

// Converts a proto "MESSAGE" into a JSON-Schema:
func (c *Converter) convertMessageType(curPkg *ProtoPackage, msg *descriptor.DescriptorProto,
	outerEnums []*descriptor.EnumDescriptorProto) (jsonschema.Type, error) {

	// Prepare a new jsonschema:
	jsonSchemaType := jsonschema.Type{
		Properties: make(map[string]*jsonschema.Type),
		Version:    jsonschema.Version,
	}
	// Generate a description from src comments (if available)
	//c.fillMessageCommentAttribute(msg, &jsonSchemaType)

	jsonSchemaType.Type = gojsonschema.TYPE_OBJECT

	// disallowAdditionalProperties will prevent validation where extra fields are found (outside of the schema):
	if c.DisallowAdditionalProperties {
		jsonSchemaType.AdditionalProperties = []byte("false")
	} else {
		jsonSchemaType.AdditionalProperties = []byte("true")
	}

	c.logger.WithField("message_str", proto.MarshalTextString(msg)).Trace("Converting message")
	for _, fieldDesc := range msg.GetField() {
		recursedJSONSchemaType, err := c.convertField(curPkg, fieldDesc, msg, outerEnums)
		c.logger.WithField("field_name", fieldDesc.GetName()).WithField("type", recursedJSONSchemaType.Type).Debug("Converted field")
		if err != nil {
			c.logger.WithError(err).WithField("field_name", fieldDesc.GetName()).WithField("message_name", msg.GetName()).Error("Failed to convert field")
			return jsonSchemaType, err
		}
		c.fillCommentAttribute(fieldDesc, recursedJSONSchemaType)
		recursedJSONSchemaType.Order = *fieldDesc.Number
		jsonSchemaType.Properties[fieldDesc.GetName()] = recursedJSONSchemaType
		if c.UseProtoAndJSONFieldnames && fieldDesc.GetName() != fieldDesc.GetJsonName() {
			jsonSchemaType.Properties[fieldDesc.GetJsonName()] = recursedJSONSchemaType
		}
	}
	return jsonSchemaType, nil
}

func (c *Converter) fillCommentAttribute(fieldDesc *descriptor.FieldDescriptorProto, recursedJSONSchemaType *jsonschema.Type) {
	if src := c.sourceInfo.GetField(fieldDesc); src != nil {
		c.fillJSONSchemaType(src, *fieldDesc.Name, recursedJSONSchemaType)
	}
}

func (c *Converter) fillMessageCommentAttribute(msgDesc *descriptor.DescriptorProto, recursedJSONSchemaType *jsonschema.Type) {
	if src := c.sourceInfo.GetMessage(msgDesc); src != nil {
		c.fillJSONSchemaType(src, *msgDesc.Name, recursedJSONSchemaType)
	}
}

func (c *Converter) fillJSONSchemaType(sl *descriptor.SourceCodeInfo_Location, name string, recursedJSONSchemaType *jsonschema.Type) {
	comments := getComments(sl)
	for k, v := range comments {
		switch strings.ToLower(k) {
		case "title":
			recursedJSONSchemaType.Title = v
		case "name":
			recursedJSONSchemaType.Title = v
		case "description":
			recursedJSONSchemaType.Description = v
		case "desc":
			recursedJSONSchemaType.Description = v
		case "format":
			recursedJSONSchemaType.Format = v
		case "fmt":
			recursedJSONSchemaType.Format = v
		case "pattern":
			recursedJSONSchemaType.Pattern = v
		case "required":
			recursedJSONSchemaType.Required = true
		case "id":
			recursedJSONSchemaType.PK = true
		case "autoincrement":
			recursedJSONSchemaType.AutoIncrement = true
			recursedJSONSchemaType.PK = true
		case "index":
			recursedJSONSchemaType.Index = true
			recursedJSONSchemaType.Required = true
		case "query":
			recursedJSONSchemaType.Query = true
		case "min":
			{
				if l, err := strconv.Atoi(v); err == nil {
					if recursedJSONSchemaType.Type == gojsonschema.TYPE_INTEGER {
						recursedJSONSchemaType.Minimum = l
					} else {
						recursedJSONSchemaType.MinLength = l
					}
				}
			}
		case "max":
			{
				if l, err := strconv.Atoi(v); err == nil {
					if recursedJSONSchemaType.Type == gojsonschema.TYPE_INTEGER {
						recursedJSONSchemaType.Maximum = l
					} else {
						recursedJSONSchemaType.MaxLength = l
					}
				}
			}
		}
	}
	if len(recursedJSONSchemaType.Title) == 0 {
		recursedJSONSchemaType.Title = ToSplitCase(name)
	}
}

func getEnumComment(sl *descriptor.SourceCodeInfo_Location) string {
	return strings.TrimSpace(sl.GetLeadingComments())
}

func formatDescription(sl *descriptor.SourceCodeInfo_Location) string {
	var lines []string
	for _, str := range sl.GetLeadingDetachedComments() {
		if s := strings.TrimSpace(str); s != ""  {
			lines = append(lines, s)
		}
	}
	if s := strings.TrimSpace(sl.GetLeadingComments()); s != ""  {
		lines = append(lines, s)
	}
	if s := strings.TrimSpace(sl.GetTrailingComments()); s != ""  {
		lines = append(lines, s)
	}
	return strings.Join(lines, "\n\n")
}

var matchFirstCap = regexp.MustCompile("(.)([A-Z][a-z]+)")
var matchAllCap = regexp.MustCompile("([a-z0-9])([A-Z])")

func ToSnakeCase(str string) string {
	snake := matchFirstCap.ReplaceAllString(str, "${1}_${2}")
	snake = matchAllCap.ReplaceAllString(snake, "${1}_${2}")
	return strings.ToLower(snake)
}

func ToSplitCase(str string) string {
	snake := matchFirstCap.ReplaceAllString(str, "${1} ${2}")
	snake = matchAllCap.ReplaceAllString(snake, "${1} ${2}")
	return snake
}

func ToCamelCase(s string) string {
	var result string
	for _, word := range strings.Split(s, "_") {
		w := []rune(word)
		w[0] = unicode.ToUpper(w[0])
		result += string(w)
	}
	return result
}

func getComments(sl *descriptor.SourceCodeInfo_Location) map[string]string {
	var lines []string
	for _, str := range sl.GetLeadingDetachedComments() {
		if s := strings.TrimSpace(str); s != "" {
			lines = append(lines, s)
		}
	}
	arr := strings.Split(sl.GetLeadingComments(), "\n")
	for _, v := range arr {
		if s := strings.TrimSpace(v); s != "" {
			lines = append(lines, s)
		}
	}
	if s := strings.TrimSpace(sl.GetTrailingComments()); s != "" {
		lines = append(lines, s)
	}
	ret := make(map[string]string)
	for _, v := range lines {
		tags := strings.Split(v, "@")
		for _, v := range tags {
			if strings.Contains(v, "//") {
				continue
			}
			v = strings.TrimSpace(v)
			arr := strings.Split(v, "=")
			if len(arr) == 1 {
				ret[strings.ToLower(arr[0])] = "true"
			} else if len(arr) == 2 {
				ret[strings.ToLower(arr[0])] = arr[1]
			}
		}
	}
	return ret
}
