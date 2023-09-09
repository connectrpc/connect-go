// Copyright 2021-2023 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connect

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/internal/assert"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestConnectErrorDetailMarshaling(t *testing.T) {
	t.Parallel()
	detail, err := NewErrorDetail(durationpb.New(time.Second))
	assert.Nil(t, err)
	data, err := json.Marshal((*connectWireDetail)(detail))
	assert.Nil(t, err)
	t.Logf("marshaled error detail: %s", string(data))

	var unmarshaled connectWireDetail
	assert.Nil(t, json.Unmarshal(data, &unmarshaled))
	assert.Equal(t, unmarshaled.wireJSON, string(data))
	assert.Equal(t, unmarshaled.pb, detail.pb)
}

func TestConnectErrorDetailMarshalingNoDescriptor(t *testing.T) {
	t.Parallel()
	raw := `{"type":"acme.user.v1.User","value":"DEADBF",` +
		`"debug":{"@type":"acme.user.v1.User","email":"someone@connectrpc.com"}}`
	var detail connectWireDetail
	assert.Nil(t, json.Unmarshal([]byte(raw), &detail))
	assert.Equal(t, detail.pb.TypeUrl, defaultAnyResolverPrefix+"acme.user.v1.User")

	_, err := (*ErrorDetail)(&detail).Value()
	assert.NotNil(t, err)
	assert.True(t, strings.HasSuffix(err.Error(), "not found"))

	encoded, err := json.Marshal(&detail)
	assert.Nil(t, err)
	assert.Equal(t, string(encoded), raw)
}

func TestConnectEndOfResponseCanonicalTrailers(t *testing.T) {
	t.Parallel()

	buffer := &bytes.Buffer{}
	endStreamMessage := newConnectEndStreamMessage(nil, make(http.Header))
	endStreamMessage.Trailer["not-canonical-header"] = []string{"a"}
	endStreamMessage.Trailer["mixed-Canonical"] = []string{"b"}
	endStreamMessage.Trailer["Mixed-Canonical"] = []string{"b"}
	endStreamMessage.Trailer["Canonical-Header"] = []string{"c"}
	err := connectMarshalEndStreamMessage(buffer, endStreamMessage)
	assert.Nil(t, err)

	output := &bytes.Buffer{}
	err = writeEnvelope(output, buffer, connectFlagEnvelopeEndStream)
	assert.Nil(t, err)

	input := &bytes.Buffer{}
	_, err = readEnvelope(input, output, -1)
	assert.Nil(t, err)

	end, err := connectUnmarshalEndStreamMessage(input, connectFlagEnvelopeEndStream)
	assert.Nil(t, err)
	assert.Equal(t, end.Trailer.Values("Not-Canonical-Header"), []string{"a"})
	assert.Equal(t, end.Trailer.Values("Mixed-Canonical"), []string{"b", "b"})
	assert.Equal(t, end.Trailer.Values("Canonical-Header"), []string{"c"})
}
