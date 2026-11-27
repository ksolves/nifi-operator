{{- define "gvList" -}}
{{- $groupVersions := . -}}

# API reference

Every field of the operator's custom resources, generated from the Go API types with
[crd-ref-docs](https://github.com/elastic/crd-ref-docs) (`make api-docs`). For an overview of
each resource (printer columns, subresources, examples) start with the
[CRD reference](crd-reference.md).

In the field tables, **Required** marks fields the API server rejects a resource without;
**Default** is the value set when the field is omitted; **Validation** lists the OpenAPI rules
(enums, patterns, limits). Rules that span several fields (CEL) are listed under the type.

## Packages
{{- range $groupVersions }}
- {{ markdownRenderGVLink . }}
{{- end }}

{{ range $groupVersions }}
{{ template "gvDetails" . }}
{{ end }}

{{- end -}}
