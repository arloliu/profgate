{{/*
The chart name, overridable, truncated to the 63 characters a label value holds.
*/}}
{{- define "profgate.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
The name every resource this chart creates is built from.
The ClusterRole and ClusterRoleBinding are cluster-scoped, so this is what
keeps two releases in one cluster from overwriting each other's RBAC.
*/}}
{{- define "profgate.fullname" -}}
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

{{- define "profgate.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "profgate.labels" -}}
helm.sh/chart: {{ include "profgate.chart" . }}
{{ include "profgate.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "profgate.selectorLabels" -}}
app.kubernetes.io/name: {{ include "profgate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "profgate.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "profgate.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "profgate.configMapName" -}}
{{- printf "%s-config" (include "profgate.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
The image reference. A digest wins over a tag, because it names one build.
*/}}
{{- define "profgate.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/*
The gateway's sizing rule for PGO collection, in bytes:
per active Collection, every in-flight sample as compressed bytes,
decompressed bytes, and a decoded profile, plus the running merged profile and
the serialized copy written to the store.
It is the formula internal/config.Config.PGOMemoryBytes applies to the same
four ceilings, so the container limit and the configuration cannot drift apart.
8 is the decode factor: a profile occupies about eight times its compressed
length once decoded.
*/}}
{{- define "profgate.pgoMemoryBytes" -}}
{{- $l := required "pgo.limits is required when pgo.enabled is true" .Values.pgo.limits -}}
{{- $parallel := int64 (required "pgo.limits.maxParallel is required to derive the memory limit" $l.maxParallel) -}}
{{- $sample := int64 (required "pgo.limits.maxSampleBytes is required to derive the memory limit" $l.maxSampleBytes) -}}
{{- $merged := int64 (required "pgo.limits.maxMergedBytes is required to derive the memory limit" $l.maxMergedBytes) -}}
{{- $active := int64 (required "pgo.limits.maxActiveCollections is required to derive the memory limit" $l.maxActiveCollections) -}}
{{- mul $active (add (mul $parallel 8 $sample) (mul 2 8 $merged)) -}}
{{- end -}}

{{/*
The container's resources block: an explicit override, else a memory limit
derived from pgo.limits, else the static limit for the interactive path.
*/}}
{{- define "profgate.resources" -}}
{{- if .Values.resources -}}
{{- toYaml .Values.resources -}}
{{- else if .Values.pgo.enabled -}}
limits:
  memory: {{ include "profgate.pgoMemoryBytes" . }}
{{- else -}}
limits:
  memory: {{ .Values.memoryLimitWithoutPGO }}
{{- end -}}
{{- end -}}

{{/*
The part of the configuration file the chart models as structured values.
The two listen addresses are fixed here because the container ports, the
readiness probe, and the Service all name them.
*/}}
{{- define "profgate.configStructured" -}}
server:
  listen: ":8080"
  opsListen: ":9090"
  logLevel: {{ .Values.server.logLevel | quote }}
  drainDelay: {{ .Values.server.drainDelay | quote }}
{{- if .Values.tls.enabled }}
  tls:
    certFile: {{ printf "%s/%s" .Values.tls.mountPath .Values.tls.certKey | quote }}
    keyFile: {{ printf "%s/%s" .Values.tls.mountPath .Values.tls.keyKey | quote }}
    minVersion: {{ .Values.tls.minVersion | quote }}
{{- end }}
auth:
  mode: disabled
  anonymousRealm: {{ .Values.auth.anonymousRealm | quote }}
realms:
{{ toYaml (required "realms must name at least one realm" .Values.realms) | indent 2 }}
{{- if .Values.pgo.enabled }}
nats:
  url: {{ required "nats.url is required when pgo.enabled is true" .Values.nats.url | quote }}
  credsFile: {{ .Values.nats.credsFile | quote }}
pgo:
  enabled: true
  configAPI: {{ .Values.pgo.configAPI | quote }}
  limits:
{{ toYaml .Values.pgo.limits | indent 4 }}
{{- end }}
{{- end -}}

{{/*
The configuration file itself: the structured keys with the raw config block
merged over them, so a key set in both takes the raw block's value.
*/}}
{{- define "profgate.config" -}}
{{- $structured := include "profgate.configStructured" . | fromYaml -}}
{{- toYaml (mergeOverwrite $structured (deepCopy (.Values.config | default dict))) -}}
{{- end -}}
