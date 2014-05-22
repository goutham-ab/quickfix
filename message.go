package quickfix

import (
	"bytes"
	"fmt"
	"github.com/quickfixgo/quickfix/fix"
	"github.com/quickfixgo/quickfix/fix/tag"
	"time"
)

//Message is a FIX Message abstraction.
type Message struct {
	Header  FieldMap
	Trailer FieldMap
	Body    FieldMap

	//ReceiveTime is the time that this message was read from the socket connection
	ReceiveTime time.Time

	//Bytes is the raw bytes of the Message
	Bytes []byte

	//slice of Bytes corresponding to the message body
	bodyBytes []byte

	//field bytes as they appear in the raw message
	fields []*fieldBytes
}

//parseError is returned when bytes cannot be parsed as a FIX message.
type parseError struct {
	OrigError string
}

func (e parseError) Error() string { return fmt.Sprintf("error parsing message: %s", e.OrigError) }

//parseMessage constructs a Message from a byte slice wrapping a FIX message.
func parseMessage(rawMessage []byte) (*Message, error) {
	var header, body, trailer fieldMap
	header.init(headerFieldOrder)
	body.init(normalFieldOrder)
	trailer.init(trailerFieldOrder)

	msg := &Message{Header: header, Body: body, Trailer: trailer, Bytes: rawMessage}

	//including required header and trailer fields, minimum of 7 fields can be expected
	//TODO: expose size for priming
	msg.fields = make([]*fieldBytes, 0, 7)

	var parsedFieldBytes *fieldBytes
	var err error

	//message must start with begin string, body length, msg type
	if parsedFieldBytes, rawMessage, err = extractSpecificField(tag.BeginString, rawMessage); err != nil {
		return nil, err
	}

	msg.fields = append(msg.fields, parsedFieldBytes)
	header.fieldLookup[parsedFieldBytes.Tag] = parsedFieldBytes

	if parsedFieldBytes, rawMessage, err = extractSpecificField(tag.BodyLength, rawMessage); err != nil {
		return nil, err
	}

	msg.fields = append(msg.fields, parsedFieldBytes)
	header.fieldLookup[parsedFieldBytes.Tag] = parsedFieldBytes

	if parsedFieldBytes, rawMessage, err = extractSpecificField(tag.MsgType, rawMessage); err != nil {
		return nil, err
	}

	msg.fields = append(msg.fields, parsedFieldBytes)
	header.fieldLookup[parsedFieldBytes.Tag] = parsedFieldBytes

	trailerBytes := []byte{}
	foundBody := false
	for {
		parsedFieldBytes, rawMessage, err = extractField(rawMessage)
		if err != nil {
			return nil, err
		}

		msg.fields = append(msg.fields, parsedFieldBytes)
		switch {
		case tag.IsHeader(parsedFieldBytes.Tag):
			header.fieldLookup[parsedFieldBytes.Tag] = parsedFieldBytes
		case tag.IsTrailer(parsedFieldBytes.Tag):
			trailer.fieldLookup[parsedFieldBytes.Tag] = parsedFieldBytes
		default:
			foundBody = true
			trailerBytes = rawMessage
			body.fieldLookup[parsedFieldBytes.Tag] = parsedFieldBytes
		}
		if parsedFieldBytes.Tag == tag.CheckSum {
			break
		}

		if !foundBody {
			msg.bodyBytes = rawMessage
		}
	}

	//body length would only be larger than trailer if fields out of order
	if len(msg.bodyBytes) > len(trailerBytes) {
		msg.bodyBytes = msg.bodyBytes[:len(msg.bodyBytes)-len(trailerBytes)]
	}

	length := 0
	for _, field := range msg.fields {
		switch field.Tag {
		case tag.BeginString, tag.BodyLength, tag.CheckSum: //tags do not contribute to length
		default:
			length += field.Length()
		}
	}

	bodyLength := new(fix.IntValue)
	msg.Header.GetField(tag.BodyLength, bodyLength)
	if bodyLength.Value != length {
		return msg, parseError{OrigError: fmt.Sprintf("Incorrect Message Length, expected %d, got %d", bodyLength.Value, length)}
	}

	return msg, nil
}

//reverseRoute returns a message builder with routing header fields initialized as the reverse of this message.
func (m *Message) reverseRoute() MessageBuilder {
	reverseBuilder := NewMessageBuilder()

	copy := func(src fix.Tag, dest fix.Tag) {
		if field := new(fix.StringValue); m.Header.GetField(src, field) == nil {
			if len(field.Value) != 0 {
				reverseBuilder.Header().SetField(dest, field)
			}
		}
	}

	copy(tag.SenderCompID, tag.TargetCompID)
	copy(tag.SenderSubID, tag.TargetSubID)
	copy(tag.SenderLocationID, tag.TargetLocationID)

	copy(tag.TargetCompID, tag.SenderCompID)
	copy(tag.TargetSubID, tag.SenderSubID)
	copy(tag.TargetLocationID, tag.SenderLocationID)

	copy(tag.OnBehalfOfCompID, tag.DeliverToCompID)
	copy(tag.OnBehalfOfSubID, tag.DeliverToSubID)
	copy(tag.DeliverToCompID, tag.OnBehalfOfCompID)
	copy(tag.DeliverToSubID, tag.OnBehalfOfSubID)

	//tags added in 4.1
	if beginString := new(fix.StringValue); m.Header.GetField(tag.BeginString, beginString) == nil {
		if beginString.Value != fix.BeginString_FIX40 {
			copy(tag.OnBehalfOfLocationID, tag.DeliverToLocationID)
			copy(tag.DeliverToLocationID, tag.OnBehalfOfLocationID)
		}
	}

	return reverseBuilder
}

func extractSpecificField(expectedTag fix.Tag, buffer []byte) (field *fieldBytes, remBuffer []byte, err error) {
	field, remBuffer, err = extractField(buffer)
	switch {
	case err != nil:
		return
	case field.Tag != expectedTag:
		err = parseError{OrigError: fmt.Sprintf("extractSpecificField: Fields out of order, expected %d, got %d", expectedTag, field.Tag)}
		return
	}

	return
}

func extractField(buffer []byte) (parsedFieldBytes *fieldBytes, remBytes []byte, err error) {
	endIndex := bytes.IndexByte(buffer, '\001')
	if endIndex == -1 {
		err = parseError{OrigError: "extractField: No Trailing Delim in " + string(buffer)}
		remBytes = buffer
		return
	}

	parsedFieldBytes, err = parseField(buffer[:endIndex+1])
	return parsedFieldBytes, buffer[(endIndex + 1):], err
}

func (m *Message) String() string {
	return string(m.Bytes)
}

func newCheckSum(value int) *fix.StringField {
	return fix.NewStringField(tag.CheckSum, fmt.Sprintf("%03d", value))
}

func (m *Message) rebuild() {
	header := m.Header.(fieldMap)
	trailer := m.Trailer.(fieldMap)

	bodyLength := header.length() + len(m.bodyBytes) + trailer.length()
	checkSum := header.total() + trailer.total()
	for _, b := range m.bodyBytes {
		checkSum += int(b)
	}
	checkSum %= 256

	header.Set(fix.NewIntField(tag.BodyLength, bodyLength))
	trailer.Set(newCheckSum(checkSum))

	var b bytes.Buffer
	header.write(&b)
	b.Write(m.bodyBytes)
	trailer.write(&b)

	m.Bytes = b.Bytes()
}
