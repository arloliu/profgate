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
{{- if include "profgate.boolValue" (dict "key" "serviceAccount.create" "value" .Values.serviceAccount.create) -}}
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
Whether one boolean toggle is on: "true", or "" when it is off.
Every template conditional that reads a values toggle reads it through this
helper, because a template conditional treats any non-empty string as true:
a quoted "false" -- --set-string, or a quoted values-file scalar -- would
render whatever the toggle gates as enabled while an operator reads it as
disabled. Only an actual boolean may pass.
Called with a dict of "key", the values key for the message, and "value".
*/}}
{{- define "profgate.boolValue" -}}
{{- if not (kindIs "bool" .value) -}}
{{- fail (printf "%s %v has type %s, not bool: any non-empty string is true to a template conditional, so a quoted \"false\" would render what it gates as enabled; set an unquoted true or false (--set, not --set-string)" .key .value (kindOf .value)) -}}
{{- end -}}
{{- if .value -}}true{{- end -}}
{{- end -}}

{{/*
Whether PGO collection is on: "true", or "" when it is off.
pgo.enabled decides the derived memory limit, the nats and pgo blocks in the
rendered configuration, and the credentials mount, so every template reads it
through this helper, which holds it to an actual boolean.
*/}}
{{- define "profgate.pgoEnabled" -}}
{{- include "profgate.boolValue" (dict "key" "pgo.enabled" "value" .Values.pgo.enabled) -}}
{{- end -}}

{{/*
Whether HTTPS on the API port is on: "true", or "" when it is off.
tls.enabled decides the certificate mount and the server.tls block in the
rendered configuration, so every template reads it through this helper, which
holds it to an actual boolean.
*/}}
{{- define "profgate.tlsEnabled" -}}
{{- include "profgate.boolValue" (dict "key" "tls.enabled" "value" .Values.tls.enabled) -}}
{{- end -}}

{{/*
Whether the authentication Secret is mounted: "true", or "" when it is off.
auth.secret.enabled decides the Secret volume, its mount, and every derived
file path in the rendered configuration -- auth.basic.usersFile,
auth.oidc.caFile, auth.oidc.browser.clientSecretFile, and
auth.oidc.browser.cookieKeyFile -- so every template reads it through this
helper, which holds it to an actual boolean.
*/}}
{{- define "profgate.authSecretEnabled" -}}
{{- include "profgate.boolValue" (dict "key" "auth.secret.enabled" "value" .Values.auth.secret.enabled) -}}
{{- end -}}

{{/*
Validate one of the four pgo.limits ceilings the memory limit is derived
from, and print it as the plain integer both the arithmetic and the rendered
configuration carry.
The gateway reads each ceiling as an integer and holds it to a range at
startup, so a value of another type, a fraction, or a value outside the range
renders a Deployment whose Pod exits at startup -- and a coerced check would
size the memory limit from a number the configuration does not carry.
Helm delivers a number from a values file or --set-json as float64, so an
integral float64 passes and is printed as the integer it holds; only a value
at most 2^53, the largest count a float64 holds exactly, can pass, because a
larger one has already lost the digits the arithmetic would use.
An int or int64, from --set, is held to the same 2^53 cap before anything
converts it to float64: converting first would round a larger value to the
nearest float64 and let a cap check on the converted number pass a value the
render then silently changes.
Called with a dict of "key", the values key under pgo.limits; "value";
"min" and "max", the bounds the gateway accepts (max 0 means no upper
bound); and "range", the same bounds as prose for the failure message.
*/}}
{{- define "profgate.pgoCeiling" -}}
{{- $v := .value -}}
{{- if kindIs "invalid" $v -}}
{{- fail (printf "pgo.limits.%s is required when pgo.enabled is true: the memory limit and the rendered configuration are derived from it" .key) -}}
{{- end -}}
{{- if or (kindIs "int" $v) (kindIs "int64" $v) -}}
{{- if or (gt (int64 $v) (int64 9007199254740992)) (lt (int64 $v) (int64 -9007199254740992)) -}}
{{- fail (printf "pgo.limits.%s %v is larger than the count a number value carries exactly, so lower it" .key $v) -}}
{{- end -}}
{{- else if kindIs "float64" $v -}}
{{- if ne $v (floor $v) -}}
{{- fail (printf "pgo.limits.%s %v is not a whole number: the gateway reads an integer and the memory limit is derived from it, so set one" .key $v) -}}
{{- end -}}
{{- if or (gt $v 9007199254740992.0) (lt $v -9007199254740992.0) -}}
{{- fail (printf "pgo.limits.%s %v is larger than the count a number value carries exactly, so lower it" .key $v) -}}
{{- end -}}
{{- else -}}
{{- fail (printf "pgo.limits.%s %v has type %s, not a number: the gateway reads an integer and the memory limit is derived from it, so set an unquoted whole number (--set or --set-json, not --set-string)" .key $v (kindOf $v)) -}}
{{- end -}}
{{- $f := float64 $v -}}
{{- if or (lt $f (float64 .min)) (and .max (gt $f (float64 .max))) -}}
{{- fail (printf "pgo.limits.%s %v must be %s: startup validation holds it there, so rendering it would deploy a gateway that exits at startup" .key $v .range) -}}
{{- end -}}
{{- int64 $f -}}
{{- end -}}

{{/*
The four pgo.limits ceilings the chart couples to something else it renders,
validated and printed as a YAML mapping of plain integers.
profgate.pgoMemoryBytes and profgate.configStructured both read this one
mapping, so the memory limit and the rendered configuration cannot be sized
from different values. The bounds mirror the validate tags on
internal/config.PGOLimits.
*/}}
{{- define "profgate.pgoLimitsValidated" -}}
{{- $l := required "pgo.limits is required when pgo.enabled is true" .Values.pgo.limits -}}
{{- if not (kindIs "map" $l) -}}
{{- fail (printf "pgo.limits %v has type %s, not a mapping: set the keys under pgo.limits" $l (kindOf $l)) -}}
{{- end -}}
maxParallel: {{ include "profgate.pgoCeiling" (dict "key" "maxParallel" "value" $l.maxParallel "min" 1 "max" 64 "range" "between 1 and 64") }}
maxSampleBytes: {{ include "profgate.pgoCeiling" (dict "key" "maxSampleBytes" "value" $l.maxSampleBytes "min" 1048576 "max" 268435456 "range" "between 1048576 (1MiB) and 268435456 (256MiB)") }}
maxMergedBytes: {{ include "profgate.pgoCeiling" (dict "key" "maxMergedBytes" "value" $l.maxMergedBytes "min" 1 "max" 1073741824 "range" "between 1 and 1073741824 (1GiB)") }}
maxActiveCollections: {{ include "profgate.pgoCeiling" (dict "key" "maxActiveCollections" "value" $l.maxActiveCollections "min" 1 "max" 0 "range" "at least 1") }}
{{- end -}}

{{/*
The gateway's sizing rule for PGO collection, in bytes:
per active Collection, every in-flight sample as compressed bytes,
decompressed bytes, and a decoded profile, plus the running merged profile and
the serialized copy written to the store.
It is the formula internal/config.Config.PGOMemoryBytes applies to the same
four ceilings, so the container limit and the configuration cannot drift
apart; the ceilings arrive through profgate.pgoLimitsValidated, the values
the rendered configuration carries.
8 is the decode factor: a profile occupies about eight times its compressed
length once decoded.
maxActiveCollections has no upper bound of its own, so the product is checked
against what a 64-bit byte count holds before it is formed; without that, a
huge ceiling would render a nonsense limit such as a negative number.
*/}}
{{- define "profgate.pgoMemoryBytes" -}}
{{- $l := include "profgate.pgoLimitsValidated" . | fromYaml -}}
{{- $parallel := int64 $l.maxParallel -}}
{{- $sample := int64 $l.maxSampleBytes -}}
{{- $merged := int64 $l.maxMergedBytes -}}
{{- $active := int64 $l.maxActiveCollections -}}
{{- $perCollection := add (mul $parallel 8 $sample) (mul 2 8 $merged) -}}
{{- if gt $active (div 9223372036854775807 $perCollection) -}}
{{- fail (printf "pgo.limits sizes a memory limit larger than a 64-bit byte count holds: maxActiveCollections %d x (maxParallel %d x 8 x maxSampleBytes %d + 2 x 8 x maxMergedBytes %d) overflows, so lower the ceilings" $active $parallel $sample $merged) -}}
{{- end -}}
{{- mul $active $perCollection -}}
{{- end -}}

{{/*
The container's resources block: an explicit override, else a memory limit
derived from pgo.limits, else the static limit for the interactive path.
*/}}
{{- define "profgate.resources" -}}
{{- if .Values.resources -}}
{{- toYaml .Values.resources -}}
{{- else if include "profgate.pgoEnabled" . -}}
limits:
  memory: {{ include "profgate.pgoMemoryBytes" . }}
{{- else -}}
limits:
  memory: {{ .Values.memoryLimitWithoutPGO }}
{{- end -}}
{{- end -}}

{{/*
Reject a Secret mount part that is not a string.
Every part of an active mount -- the Secret name, the mount path, the data
keys, and nats.credsFile -- is rendered verbatim into a path or a Deployment
field, and rendering formats a number or a boolean differently from the
string validation reads, so only a value that is already a string may pass.
Called with a dict of "key", the values key for the message, and "value".
*/}}
{{- define "profgate.mountPartString" -}}
{{- if not (kindIs "string" .value) -}}
{{- fail (printf "%s %v has type %s, not string: the chart renders the value verbatim, so quote it in the values file or set it with --set-string" .key .value (kindOf .value)) -}}
{{- end -}}
{{- end -}}

{{/*
Reject a Secret name Kubernetes would refuse.
A Secret name is a DNS-1123 subdomain: at most 253 characters of lowercase
letters, digits, '-', and '.', each dot-separated part starting and ending
with a letter or digit. Anything else renders a Deployment the API server
rejects. The value must also already be a string: YAML types a bare name
like "true" as a boolean, which would render an undecodable secretName.
Called with a dict of "key", the values key for the message, and "value".
*/}}
{{- define "profgate.secretName" -}}
{{- include "profgate.mountPartString" . -}}
{{- if or (gt (len .value) 253) (not (regexMatch `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$` .value)) -}}
{{- fail (printf "%s %q is not a name a Secret can carry: a Secret name is a DNS-1123 subdomain -- at most 253 characters of lowercase letters, digits, '-', and '.', each dot-separated part starting and ending with a letter or digit" .key .value) -}}
{{- end -}}
{{- end -}}

{{/*
Reject a Secret data key Kubernetes would refuse.
A Secret data key is at most 253 characters of letters, digits, '-', '_',
and '.', and is neither "." nor a key starting with ".." -- the names the
kubelet cannot project into a file. Anything else renders a Deployment the
API server rejects, and "." would additionally clean-join with a mount path
into the mount directory itself, so a directory credsFile could pass the
join check while naming no file. The value must also already be a string,
the way every mount part must.
Called with a dict of "key", the values key for the message, and "value".
*/}}
{{- define "profgate.secretDataKey" -}}
{{- include "profgate.mountPartString" . -}}
{{- if or (gt (len .value) 253) (not (regexMatch "^[-._a-zA-Z0-9]+$" .value)) (eq .value ".") (hasPrefix ".." .value) -}}
{{- fail (printf "%s %q is not a Secret data key: Kubernetes holds a data key to at most 253 characters of letters, digits, '-', '_', and '.', and refuses '.', '..', and any key starting with '..'" .key .value) -}}
{{- end -}}
{{- end -}}

{{/*
Reject the escape hatches for the keys the Deployment couples to something
else it renders: the derived memory limit and the Secret mounts.
The raw config block is merged over the structured values after
profgate.resources has read .Values.pgo, and extraEnv overrides the file at
runtime, so pgo.enabled or a sizing ceiling arriving through either hatch
would run PGO under a limit sized for different ceilings. A null or scalar
config.pgo or config.pgo.limits is the same hatch in bulk: the merge copies
it over the structured mapping, the rendered file carries a null the binary
reads as absent, and the binary's own defaults replace the values the memory
limit was sized for. An empty mapping is harmless -- the merge leaves the
structured values in place -- and every other key, pgo.configAPI included,
stays free to override through both hatches.
The file-path keys are the second coupling: the credentials volume follows
nats.credsFile and the certificate volume follows tls.enabled, so
nats.credsFile or the server.tls certificate paths arriving through either
hatch can name files nothing mounts, and startup validation ends the Pod
over the missing file. server.tls.minVersion names no file and stays free.
A null or scalar config.server, config.server.tls, or config.nats is the
bulk form again: the merge copies it over the structured mapping and nulls
out the listen addresses the container ports and probe are built around, or
the certificate and credentials paths the mounted Secrets back, so those
shapes are refused the way config.pgo is.
*/}}
{{- define "profgate.validateNoDerivedOverrides" -}}
{{- $raw := .Values.config | default dict -}}
{{- if and (hasKey $raw "pgo") (not (kindIs "map" (get $raw "pgo"))) -}}
{{- fail "config.pgo must be a mapping: null or a scalar replaces the whole pgo block in the rendered configuration and resets pgo.enabled and the sizing ceilings the memory limit is derived from when PGO is enabled and resources is empty, so set pgo.enabled, pgo.configAPI, and the keys under pgo.limits instead" -}}
{{- end -}}
{{- $rawPgo := dig "pgo" dict $raw | default dict -}}
{{- if hasKey $rawPgo "enabled" -}}
{{- fail "config.pgo.enabled is not supported: when resources is empty, the memory limit is derived from pgo.enabled, so set pgo.enabled instead" -}}
{{- end -}}
{{- if and (hasKey $rawPgo "limits") (not (kindIs "map" (get $rawPgo "limits"))) -}}
{{- fail "config.pgo.limits must be a mapping: null or a scalar replaces the whole pgo.limits block in the rendered configuration and resets the sizing ceilings the memory limit is derived from when PGO is enabled and resources is empty, so set pgo.limits.maxParallel, pgo.limits.maxSampleBytes, pgo.limits.maxMergedBytes, and pgo.limits.maxActiveCollections instead" -}}
{{- end -}}
{{- range $key := list "maxParallel" "maxSampleBytes" "maxMergedBytes" "maxActiveCollections" -}}
{{- if hasKey (dig "limits" dict $rawPgo | default dict) $key -}}
{{- fail (printf "config.pgo.limits.%s is not supported: when PGO is enabled and resources is empty, the memory limit is derived from pgo.limits.%s, so set pgo.limits.%s instead" $key $key $key) -}}
{{- end -}}
{{- end -}}
{{- if and (hasKey $raw "server") (not (kindIs "map" (get $raw "server"))) -}}
{{- fail "config.server must be a mapping: null or a scalar replaces the whole server block in the rendered configuration and resets the listen addresses the container ports and readiness probe are built around, so set the individual keys under config.server instead" -}}
{{- end -}}
{{- $rawServer := dig "server" dict $raw | default dict -}}
{{- if and (hasKey $rawServer "tls") (not (kindIs "map" (get $rawServer "tls"))) -}}
{{- fail "config.server.tls must be a mapping: null or a scalar replaces the whole server.tls block in the rendered configuration and drops the certificate paths the certificate mount is coupled to, so set tls.enabled, tls.existingSecret, and tls.minVersion instead" -}}
{{- end -}}
{{- range $key := list "certFile" "keyFile" -}}
{{- if hasKey (dig "tls" dict $rawServer | default dict) $key -}}
{{- fail (printf "config.server.tls.%s is not supported: the certificate mount follows tls.enabled and tls.existingSecret, so a path set here can name a file nothing mounts; set tls.enabled and tls.existingSecret instead" $key) -}}
{{- end -}}
{{- end -}}
{{- if and (hasKey $raw "nats") (not (kindIs "map" (get $raw "nats"))) -}}
{{- fail "config.nats must be a mapping: null or a scalar replaces the whole nats block in the rendered configuration and drops the URL and the credentials path the credentials mount is coupled to, so set nats.url, nats.credsFile, and nats.existingSecret instead" -}}
{{- end -}}
{{- if hasKey (dig "nats" dict $raw | default dict) "credsFile" -}}
{{- fail "config.nats.credsFile is not supported: the credentials mount follows nats.credsFile, so a path set here can name a file nothing mounts; set nats.credsFile and nats.existingSecret instead" -}}
{{- end -}}
{{- /* The four authentication file paths are the same coupling: each is
derived from auth.secret.mountPath and a Secret data key, so a path arriving
through the raw block can name a file the auth Secret does not mount. A
scalar or malformed config.auth, config.auth.basic, config.auth.oidc, or
config.auth.oidc.browser folds to an empty mapping here rather than failing
on its own -- the four leaf keys below are the coupling the chart guards, not
the shape of the blocks around them. */ -}}
{{- $rawAuth := dict -}}
{{- if kindIs "map" (dig "auth" dict $raw) -}}{{- $rawAuth = dig "auth" dict $raw -}}{{- end -}}
{{- $rawAuthBasic := dict -}}
{{- if kindIs "map" (dig "basic" dict $rawAuth) -}}{{- $rawAuthBasic = dig "basic" dict $rawAuth -}}{{- end -}}
{{- if hasKey $rawAuthBasic "usersFile" -}}
{{- fail "config.auth.basic.usersFile is not supported: the users file mount follows auth.basic.usersFile and auth.secret.mountPath, so a path set here can name a file nothing mounts; set auth.basic.usersFile and auth.secret.mountPath instead" -}}
{{- end -}}
{{- $rawAuthOIDC := dict -}}
{{- if kindIs "map" (dig "oidc" dict $rawAuth) -}}{{- $rawAuthOIDC = dig "oidc" dict $rawAuth -}}{{- end -}}
{{- if hasKey $rawAuthOIDC "caFile" -}}
{{- fail "config.auth.oidc.caFile is not supported: the CA certificate mount follows auth.oidc.caKey and auth.secret.mountPath, so a path set here can name a file nothing mounts; set auth.oidc.caKey and auth.secret.mountPath instead" -}}
{{- end -}}
{{- $rawAuthBrowser := dict -}}
{{- if kindIs "map" (dig "browser" dict $rawAuthOIDC) -}}{{- $rawAuthBrowser = dig "browser" dict $rawAuthOIDC -}}{{- end -}}
{{- if hasKey $rawAuthBrowser "clientSecretFile" -}}
{{- fail "config.auth.oidc.browser.clientSecretFile is not supported: the client secret mount follows auth.oidc.browser.clientSecretFile and auth.secret.mountPath, so a path set here can name a file nothing mounts; set auth.oidc.browser.clientSecretFile and auth.secret.mountPath instead" -}}
{{- end -}}
{{- if hasKey $rawAuthBrowser "cookieKeyFile" -}}
{{- fail "config.auth.oidc.browser.cookieKeyFile is not supported: the cookie key mount follows auth.secret.mountPath, so a path set here can name a file nothing mounts; set auth.secret.mountPath instead" -}}
{{- end -}}
{{- /* Each Secret mount is assembled from parts -- a Secret name, a mount
path, and data keys -- and rendering holds every part of an active mount to
what Kubernetes accepts: an empty Secret name or mount path renders a
Deployment the API server rejects, a relative mount path is nowhere a
volume can mount, and an empty or malformed data key joins into a path
naming the mount directory instead of a file. Every part must already be a
string, because the values here are what the Deployment and the
configuration render verbatim: coercing a number or a boolean for the check
would pass a value that renders as something else. The string check reads
each raw value, with only a null folded to "": a `default` before it would
fold a boolean false or a zero to "" too, and the failure would then name
an empty value the values file does not carry -- or, for nats.credsFile,
silently select the no-credentials path. */ -}}
{{- $pgoEnabled := include "profgate.pgoEnabled" . -}}
{{- $credsFile := .Values.nats.credsFile -}}
{{- if kindIs "invalid" $credsFile -}}{{- $credsFile = "" -}}{{- end -}}
{{- if $pgoEnabled -}}
{{- include "profgate.mountPartString" (dict "key" "nats.credsFile" "value" $credsFile) -}}
{{- end -}}
{{- if and $pgoEnabled $credsFile -}}
{{- /* The Secret name gets the same raw-value treatment: only a null folds
to "", and the string-kind check runs before the emptiness check, so a
boolean false or a zero fails naming the value and its type rather than as
"empty", a value the values file does not carry. */ -}}
{{- $natsSecret := .Values.nats.existingSecret -}}
{{- if kindIs "invalid" $natsSecret -}}{{- $natsSecret = "" -}}{{- end -}}
{{- include "profgate.mountPartString" (dict "key" "nats.existingSecret" "value" $natsSecret) -}}
{{- if not $natsSecret -}}
{{- fail "nats.existingSecret is empty: when PGO is enabled and nats.credsFile is set, the credentials volume mounts that Secret, so name the Secret holding the NATS credentials" -}}
{{- end -}}
{{- include "profgate.secretName" (dict "key" "nats.existingSecret" "value" $natsSecret) -}}
{{- $natsMount := .Values.nats.mountPath -}}
{{- if kindIs "invalid" $natsMount -}}{{- $natsMount = "" -}}{{- end -}}
{{- include "profgate.mountPartString" (dict "key" "nats.mountPath" "value" $natsMount) -}}
{{- if not (hasPrefix "/" $natsMount) -}}
{{- fail (printf "nats.mountPath %q is not an absolute path: the credentials Secret is mounted there, so it must start with /" $natsMount) -}}
{{- end -}}
{{- $natsKey := .Values.nats.secretKey -}}
{{- if kindIs "invalid" $natsKey -}}{{- $natsKey = "" -}}{{- end -}}
{{- include "profgate.secretDataKey" (dict "key" "nats.secretKey" "value" $natsKey) -}}
{{- end -}}
{{- if include "profgate.tlsEnabled" . -}}
{{- $tlsSecret := .Values.tls.existingSecret -}}
{{- if kindIs "invalid" $tlsSecret -}}{{- $tlsSecret = "" -}}{{- end -}}
{{- include "profgate.mountPartString" (dict "key" "tls.existingSecret" "value" $tlsSecret) -}}
{{- if not $tlsSecret -}}
{{- fail "tls.existingSecret is empty: tls.enabled mounts that Secret, so name the kubernetes.io/tls Secret holding the certificate" -}}
{{- end -}}
{{- include "profgate.secretName" (dict "key" "tls.existingSecret" "value" $tlsSecret) -}}
{{- $tlsMount := .Values.tls.mountPath -}}
{{- if kindIs "invalid" $tlsMount -}}{{- $tlsMount = "" -}}{{- end -}}
{{- include "profgate.mountPartString" (dict "key" "tls.mountPath" "value" $tlsMount) -}}
{{- if not (hasPrefix "/" $tlsMount) -}}
{{- fail (printf "tls.mountPath %q is not an absolute path: the certificate Secret is mounted there, so it must start with /" $tlsMount) -}}
{{- end -}}
{{- range $key := list "certKey" "keyKey" -}}
{{- $v := get $.Values.tls $key -}}
{{- if kindIs "invalid" $v -}}{{- $v = "" -}}{{- end -}}
{{- include "profgate.secretDataKey" (dict "key" (printf "tls.%s" $key) "value" $v) -}}
{{- end -}}
{{- end -}}
{{- /* The structured nats.credsFile can drift the same way the raw hatch
can: the Deployment mounts the Secret key nats.secretKey at nats.mountPath,
while the configuration carries nats.credsFile verbatim, so a credsFile
pointing anywhere else -- or a mountPath or secretKey moved without it --
renders a gateway that exits over a file nothing mounts. credsFile stays the
explicit value the configuration carries; this only refuses to render the
three values disagreeing. Both sides are path-cleaned before the comparison,
so spellings of the same path -- a trailing slash, a doubled slash, a "."
component -- agree, and only genuinely different paths fail. The check
follows the mount's own gate: with PGO disabled nothing mounts and the
rendered configuration carries no nats block, so inactive nats values never
block a render. */ -}}
{{- if and $pgoEnabled $credsFile -}}
{{- include "profgate.mountPartString" (dict "key" "nats.credsFile" "value" $credsFile) -}}
{{- $joinMount := .Values.nats.mountPath -}}
{{- if kindIs "invalid" $joinMount -}}{{- $joinMount = "" -}}{{- end -}}
{{- include "profgate.mountPartString" (dict "key" "nats.mountPath" "value" $joinMount) -}}
{{- $joinKey := .Values.nats.secretKey -}}
{{- if kindIs "invalid" $joinKey -}}{{- $joinKey = "" -}}{{- end -}}
{{- include "profgate.mountPartString" (dict "key" "nats.secretKey" "value" $joinKey) -}}
{{- $mounted := clean (printf "%s/%s" $joinMount $joinKey) -}}
{{- if ne (clean $credsFile) $mounted -}}
{{- fail (printf "nats.credsFile %q does not name the mounted credentials file %q: when PGO is enabled, the Deployment mounts the Secret key nats.secretKey %q at nats.mountPath %q, so the gateway can only read their join; set the three values to agree, or set nats.credsFile to \"\" to skip the JWT credentials file" $credsFile $mounted $joinKey $joinMount) -}}
{{- end -}}
{{- end -}}
{{- $guarded := dict
      "PROFGATE_PGO_ENABLED" "when resources is empty, the memory limit is derived from pgo.enabled, so set pgo.enabled instead"
      "PROFGATE_PGO_LIMIT_MAX_PARALLEL" "when PGO is enabled and resources is empty, the memory limit is derived from pgo.limits.maxParallel, so set pgo.limits.maxParallel instead"
      "PROFGATE_PGO_LIMIT_MAX_SAMPLE_BYTES" "when PGO is enabled and resources is empty, the memory limit is derived from pgo.limits.maxSampleBytes, so set pgo.limits.maxSampleBytes instead"
      "PROFGATE_PGO_LIMIT_MAX_MERGED_BYTES" "when PGO is enabled and resources is empty, the memory limit is derived from pgo.limits.maxMergedBytes, so set pgo.limits.maxMergedBytes instead"
      "PROFGATE_PGO_LIMIT_MAX_ACTIVE_COLLECTIONS" "when PGO is enabled and resources is empty, the memory limit is derived from pgo.limits.maxActiveCollections, so set pgo.limits.maxActiveCollections instead" -}}
{{- $mountGuarded := dict
      "PROFGATE_NATS_CREDS_FILE" "the credentials mount follows nats.credsFile, so set nats.credsFile and nats.existingSecret instead"
      "PROFGATE_TLS_CERT_FILE" "the certificate mount follows tls.enabled, so set tls.enabled and tls.existingSecret instead"
      "PROFGATE_TLS_KEY_FILE" "the certificate mount follows tls.enabled, so set tls.enabled and tls.existingSecret instead"
      "PROFGATE_AUTH_BASIC_USERS_FILE" "the users file mount follows auth.basic.usersFile and auth.secret.mountPath, so set those instead"
      "PROFGATE_AUTH_OIDC_CA_FILE" "the CA certificate mount follows auth.oidc.caKey and auth.secret.mountPath, so set those instead"
      "PROFGATE_AUTH_OIDC_CLIENT_SECRET_FILE" "the client secret mount follows auth.oidc.browser.clientSecretFile and auth.secret.mountPath, so set those instead"
      "PROFGATE_AUTH_OIDC_COOKIE_KEY_FILE" "the cookie key mount follows auth.secret.mountPath, so set that instead" -}}
{{- range .Values.extraEnv -}}
{{- if hasKey $guarded (.name | default "") -}}
{{- fail (printf "extraEnv must not set %s: %s" .name (get $guarded .name)) -}}
{{- end -}}
{{- if hasKey $mountGuarded (.name | default "") -}}
{{- fail (printf "extraEnv must not set %s: %s" .name (get $mountGuarded .name)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Reject a values file that names a file the authentication Secret does not
mount, or a browser login that a plaintext listener cannot carry.
auth.basic.usersFile, auth.oidc.caKey, and auth.oidc.browser.clientSecretFile
each name a Secret data key that only exists once auth.secret.enabled mounts
the Secret; a value set without the Secret enabled would render a
configuration naming a file nothing mounts, and the gateway would exit at
startup over it. The cookie key file is unconditional whenever browser is
set, so browser alone carries the same requirement.
auth.oidc.browser also requires tls.enabled: the session cookie carries
Secure and a __Host- prefix, which a plaintext listener cannot set, and
config.Load rejects the combination anyway, so failing here catches it before
a Pod ever starts.
*/}}
{{- define "profgate.validateAuthSecret" -}}
{{- $mode := .Values.auth.mode -}}
{{- $needsSecret := false -}}
{{- if eq $mode "basic" -}}
{{- if .Values.auth.basic.usersFile -}}{{- $needsSecret = true -}}{{- end -}}
{{- else if eq $mode "oidc" -}}
{{- if .Values.auth.oidc.caKey -}}{{- $needsSecret = true -}}{{- end -}}
{{- if .Values.auth.oidc.browser -}}
{{- $needsSecret = true -}}
{{- if not (include "profgate.tlsEnabled" .) -}}
{{- fail "tls.enabled must be true when auth.oidc.browser is set: the session cookie carries Secure and a __Host- prefix, which a plaintext listener cannot set" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and $needsSecret (not (include "profgate.authSecretEnabled" .)) -}}
{{- fail (printf "auth.secret.enabled must be true: %s mode is configured with a file that the Secret at auth.secret.mountPath carries" $mode) -}}
{{- end -}}
{{- end -}}

{{/*
One podDisruptionBudget bound, printed as what the budget renders: the
normalized integer or the percentage string when the bound is set, or ""
when the value is null or the empty string. A set bound has to be something
a PodDisruptionBudget can carry -- an integer from 0 through 2147483647,
the int32 range the field decodes into, or a percentage such as "50%" --
because anything else renders a budget Kubernetes cannot decode, so every
other shape fails here naming the value. A numeric bound is checked
arithmetically, not on its printed form, and is printed as the plain
integer it holds: an integral float64, which is how a values file or
--set-json delivers a number, would otherwise print in exponent notation.
Called with a dict of "key", the values key for the message, and "value".
*/}}
{{- define "profgate.pdbBound" -}}
{{- if kindIs "invalid" .value -}}
{{- else if kindIs "string" .value -}}
{{- if eq .value "" -}}
{{- else if regexMatch "^[0-9]+%$" .value -}}
{{- .value -}}
{{- else -}}
{{- fail (printf "podDisruptionBudget.%s %q is neither a non-negative integer nor a percentage such as \"50%%\"; set one of those, or clear it (\"\" and null both mean unset)" .key .value) -}}
{{- end -}}
{{- else if or (kindIs "int" .value) (kindIs "int64" .value) (kindIs "float64" .value) -}}
{{- $f := float64 .value -}}
{{- if or (ne $f (floor $f)) (lt $f 0.0) (gt $f 2147483647.0) -}}
{{- fail (printf "podDisruptionBudget.%s %v is neither a non-negative integer at most 2147483647, the largest count the field decodes, nor a percentage such as \"50%%\"; set one of those, or clear it (\"\" and null both mean unset)" .key .value) -}}
{{- end -}}
{{- printf "%d" (int64 $f) -}}
{{- else -}}
{{- fail (printf "podDisruptionBudget.%s is a %s, and a disruption bound is a non-negative integer or a percentage such as \"50%%\"; set one of those, or clear it (\"\" and null both mean unset)" .key (kindOf .value)) -}}
{{- end -}}
{{- end -}}

{{/*
The NATS URL the gateway reads. A url key in the raw config block wins over
nats.url by presence, not by value: the merge below copies the raw block over
the structured keys, so even an empty raw url is what reaches the gateway.
Resolving it here, with the same presence rule and a non-empty requirement on
whichever value is effective, is what keeps the requiredness check and the
NOTES.txt URL in agreement with the file the ConfigMap actually carries.
The effective value must already be a string -- `required` treats a boolean
or a number as present, and the merge would render it into a field the
gateway reads as a string -- and it is held to the shape startup validation
holds it to: comma-separated URLs, each beginning with nats:// or tls://.
*/}}
{{- define "profgate.natsURL" -}}
{{- $rawNats := dig "nats" dict (.Values.config | default dict) -}}
{{- if not (kindIs "map" $rawNats) -}}{{- $rawNats = dict -}}{{- end -}}
{{- $url := "" -}}
{{- $urlKey := "nats.url" -}}
{{- if hasKey $rawNats "url" -}}
{{- $urlKey = "config.nats.url" -}}
{{- if not (kindIs "string" (get $rawNats "url")) -}}
{{- fail (printf "config.nats.url %v has type %s, not string: the gateway reads a comma-separated URL string, so quote it in the values file or set it with --set-string" (get $rawNats "url") (kindOf (get $rawNats "url"))) -}}
{{- end -}}
{{- $url = required "config.nats.url is empty: a url key in the raw config block replaces nats.url in the rendered configuration, so set it to the NATS URL or remove the key and set nats.url" (get $rawNats "url") -}}
{{- else -}}
{{- if and (not (kindIs "invalid" .Values.nats.url)) (not (kindIs "string" .Values.nats.url)) -}}
{{- fail (printf "nats.url %v has type %s, not string: the gateway reads a comma-separated URL string, so quote it in the values file or set it with --set-string" .Values.nats.url (kindOf .Values.nats.url)) -}}
{{- end -}}
{{- $url = required "nats.url is required when pgo.enabled is true: set nats.url or config.nats.url" .Values.nats.url -}}
{{- end -}}
{{- range $entry := splitList "," $url -}}
{{- if not (or (hasPrefix "nats://" (trim $entry)) (hasPrefix "tls://" (trim $entry))) -}}
{{- fail (printf "%s %q: every comma-separated URL must begin with nats:// or tls://, the schemes the gateway accepts" $urlKey (trim $entry)) -}}
{{- end -}}
{{- end -}}
{{- $url -}}
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
{{- if include "profgate.tlsEnabled" . }}
  tls:
    certFile: {{ printf "%s/%s" .Values.tls.mountPath .Values.tls.certKey | quote }}
    keyFile: {{ printf "%s/%s" .Values.tls.mountPath .Values.tls.keyKey | quote }}
    minVersion: {{ .Values.tls.minVersion | quote }}
{{- end }}
auth:
  mode: {{ .Values.auth.mode | quote }}
{{- if eq .Values.auth.mode "disabled" }}
  anonymousRealm: {{ .Values.auth.anonymousRealm | quote }}
{{- else if eq .Values.auth.mode "basic" }}
  basic:
    {{- with .Values.auth.basic.users }}
    users:
{{ toYaml . | indent 6 }}
    {{- end }}
    {{- if .Values.auth.basic.usersFile }}
    usersFile: {{ printf "%s/%s" .Values.auth.secret.mountPath .Values.auth.basic.usersFile | quote }}
    {{- end }}
    allowPlaintext: {{ .Values.auth.basic.allowPlaintext }}
    maxConcurrent: {{ .Values.auth.basic.maxConcurrent }}
{{- else if eq .Values.auth.mode "oidc" }}
  oidc:
    issuer: {{ .Values.auth.oidc.issuer | quote }}
    audience: {{ .Values.auth.oidc.audience | quote }}
    tokenType: {{ .Values.auth.oidc.tokenType | quote }}
    usernameClaim: {{ .Values.auth.oidc.usernameClaim | quote }}
    groupsClaim: {{ .Values.auth.oidc.groupsClaim | quote }}
    {{- if .Values.auth.oidc.caKey }}
    caFile: {{ printf "%s/%s" .Values.auth.secret.mountPath .Values.auth.oidc.caKey | quote }}
    {{- end }}
    {{- if .Values.auth.oidc.httpProxy }}
    httpProxy: {{ .Values.auth.oidc.httpProxy | quote }}
    {{- end }}
    mapping:
{{ toYaml .Values.auth.oidc.mapping | indent 6 }}
    {{- if .Values.auth.oidc.browser }}
    browser:
      clientID: {{ .Values.auth.oidc.browser.clientID | quote }}
      redirectURL: {{ .Values.auth.oidc.browser.redirectURL | quote }}
      {{- if .Values.auth.oidc.browser.clientSecretFile }}
      clientSecretFile: {{ printf "%s/%s" .Values.auth.secret.mountPath .Values.auth.oidc.browser.clientSecretFile | quote }}
      {{- end }}
      {{- with .Values.auth.oidc.browser.scopes }}
      scopes:
{{ toYaml . | indent 8 }}
      {{- end }}
      cookieKeyFile: {{ printf "%s/cookie.key" .Values.auth.secret.mountPath | quote }}
      {{- with .Values.auth.oidc.browser.sessionTTL }}
      sessionTTL: {{ . | quote }}
      {{- end }}
      {{- with .Values.auth.oidc.browser.transactionTTL }}
      transactionTTL: {{ . | quote }}
      {{- end }}
    {{- end }}
{{- end }}
realms:
{{ toYaml (required "realms must name at least one realm" .Values.realms) | indent 2 }}
{{- if include "profgate.pgoEnabled" . }}
nats:
  url: {{ include "profgate.natsURL" . | quote }}
{{- if .Values.nats.credsFile }}
  credsFile: {{ .Values.nats.credsFile | quote }}
{{- end }}
pgo:
  enabled: true
  configAPI: {{ .Values.pgo.configAPI | quote }}
  {{- /* The four ceilings come from profgate.pgoLimitsValidated, the same
  values the memory limit is derived from; any other pgo.limits key is
  rendered as written. */}}
  limits:
{{ toYaml (mergeOverwrite (deepCopy (default dict .Values.pgo.limits)) (include "profgate.pgoLimitsValidated" . | fromYaml)) | indent 4 }}
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
