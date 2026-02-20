// Copyright 2025 Lightpanda (Selecy SAS)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mcp

type Schema any

type SchemaType struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type schemaString SchemaType

func NewSchemaString(description string) schemaString {
	return schemaString(SchemaType{Type: "string", Description: description})
}

type Properties map[string]Schema

type schemaObject struct {
	SchemaType
	Properties           Properties `json:"properties"`
	Required             []string   `json:"required,omitempty"`
	AdditionalProperties bool       `json:"additionalProperties"`
}

// NewSchemaObject creates a JSON Schema object with properties and optional required fields.
// The required parameter is variadic for backward compatibility.
func NewSchemaObject(p map[string]Schema, required ...string) schemaObject {
	return schemaObject{
		SchemaType: SchemaType{Type: "object"},
		Properties: p,
		Required:   required,
	}
}

type Tool struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	InputSchema schemaObject `json:"inputSchema"`
	// TODO annotations
}
