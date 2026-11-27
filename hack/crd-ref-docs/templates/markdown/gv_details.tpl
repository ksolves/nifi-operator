{{- define "gvDetails" -}}
{{- $gv := . -}}

## {{ $gv.GroupVersionString }}

{{ $gv.Doc }}

{{- if $gv.Kinds  }}
### Resource types
{{- range $gv.SortedKinds }}
- {{ $gv.TypeForKind . | markdownRenderTypeLink }}
{{- end }}
{{ end }}

{{- range $gv.SortedKinds }}
{{ template "type" ($gv.TypeForKind .) }}
{{- end }}

### Other types
{{ range $gv.SortedTypes }}
{{- if not .GVK }}
{{ template "type" . }}
{{- end }}
{{- end }}

{{- end -}}
