{{- define "type" -}}
{{- $type := . -}}
{{- if markdownShouldRenderType $type -}}

#### {{ $type.Name }}

{{ if $type.IsAlias }}_Underlying type:_ _{{ markdownRenderTypeLink $type.UnderlyingType  }}_{{ end }}

{{ $type.Doc }}

{{ if $type.Validation -}}
_Rules:_
{{- range $type.Validation }}
- {{ . }}
{{- end }}
{{- end }}

{{ if $type.References -}}
_Appears in:_
{{- range $type.SortedReferences }}
- {{ markdownRenderTypeLink . }}
{{- end }}
{{- end }}

{{ if $type.Members -}}
| Field | Type | Required | Default | Description | Validation |
| --- | --- | --- | --- | --- | --- |
{{ if $type.GVK -}}
| `apiVersion` | _string_ | ✓ | | `{{ $type.GVK.Group }}/{{ $type.GVK.Version }}` | |
| `kind` | _string_ | ✓ | | `{{ $type.GVK.Kind }}` | |
{{ end -}}

{{ range $type.Members -}}
{{- $required := false -}}
{{- range .Validation }}{{ if hasPrefix "Required" . }}{{ $required = true }}{{ end }}{{ end -}}
| <a id="{{ lower $type.Name }}-{{ lower .Name }}"></a>`{{ .Name }}` | _{{ markdownRenderType .Type }}_ | {{ if $required }}✓{{ end }} | {{ markdownRenderDefault .Default }} | {{ template "type_members" . }} | {{ range .Validation -}}{{ if not (or (hasPrefix "Required" .) (hasPrefix "Optional" .)) }}{{ markdownRenderFieldDoc . }} <br />{{ end }}{{ end }} |
{{ end -}}

{{ end -}}

{{ if $type.EnumValues -}}
| Value | Description |
| --- | --- |
{{ range $type.EnumValues -}}
| `{{ .Name }}` | {{ markdownRenderFieldDoc .Doc }} |
{{ end -}}
{{ end -}}


{{- end -}}
{{- end -}}
