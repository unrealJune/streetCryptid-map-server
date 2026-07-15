{{/* Common naming and label helpers. */}}

{{- define "scms.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "scms.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "scms.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "scms.labels" -}}
helm.sh/chart: {{ include "scms.chart" . }}
{{ include "scms.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: streetcryptid
{{- end -}}

{{- define "scms.selectorLabels" -}}
app.kubernetes.io/name: {{ include "scms.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "scms.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "scms.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The Martin source id derives from the PMTiles filename stem: planet. */}}
{{- define "scms.martinSource" -}}planet{{- end -}}

{{/* Internal Martin base URL the API reaches over localhost. */}}
{{- define "scms.martinURL" -}}
http://127.0.0.1:{{ .Values.martin.port }}/{{ include "scms.martinSource" . }}
{{- end -}}

{{/* Fully qualified API image ref, pinned by digest when provided. */}}
{{- define "scms.apiImage" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{/* Shared env for bootstrap + updater (tiles subcommands). */}}
{{- define "scms.tilesEnv" -}}
- name: TILE_DATA_DIR
  value: {{ .Values.tiles.dataDir | quote }}
{{- if .Values.tiles.manifestURL }}
- name: TILE_MANIFEST_URL
  value: {{ .Values.tiles.manifestURL | quote }}
{{- end }}
{{- if .Values.tiles.publicKey }}
- name: TILE_MANIFEST_PUBLIC_KEY_FILE
  value: /config/tiles-manifest.pub
{{- end }}
{{- if .Values.tiles.authSecret.name }}
- name: TILE_AUTH_TOKEN_FILE
  value: /secrets/{{ .Values.tiles.authSecret.key }}
{{- end }}
- name: TILE_UPDATE_INTERVAL
  value: {{ .Values.tiles.autoUpdate.interval | quote }}
- name: TILE_RETAIN_RELEASES
  value: {{ .Values.tiles.retainReleases | quote }}
{{- end -}}

{{/* Name of the tiles data PVC (chart-created or existing). */}}
{{- define "scms.tilesClaimName" -}}
{{- if .Values.persistence.tiles.existingClaim -}}
{{- .Values.persistence.tiles.existingClaim -}}
{{- else -}}
{{- printf "%s-tiles" (include "scms.fullname" .) -}}
{{- end -}}
{{- end -}}
