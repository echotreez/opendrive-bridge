package cli

// The service definitions `odctl daemon install` writes.
//
// # Why these exist at all
//
// kardianos/service generates a perfectly good unit and plist, and until the
// caching gateway arrived there was no reason to override either. There is now,
// and it is a one-line reason with a serious consequence: **neither generated
// template sets a stop timeout.**
//
// With write-back on, SIGTERM is the daemon's cue to flush files OpenDrive does
// not have yet (§3.5.2 rule 4), and it allows itself two minutes for that by
// default. The platform defaults are shorter than that — launchd kills after
// twenty seconds, systemd after ninety — so `odctl daemon stop` on a bridge with
// a few large files outstanding would have the process killed part-way through the
// drain.
//
// That is bad twice over. The obvious half is that the upload does not finish. The
// worse half is that the drain's last act, when it runs out of time, is to write
// one log line per unfinished file naming where its data is — and a process that
// has been SIGKILLed writes nothing. The user would be left with data loss and no
// record of what was lost, which is the precise failure the rule was written to
// prevent.
//
// So both templates are the library's own with one directive added. They are
// copies, which means they can drift from the library's if it changes; the
// alternative was patching a generated file after the fact, and a copy that is
// visibly a copy seemed the lesser evil. If a future version of the library grows
// a stop-timeout option, these should go.
//
// # What these are not
//
// They are not the hardened unit in deploy/systemd/opendrived.service. That file
// is for somebody installing by hand and carries the full sandbox — ProtectSystem,
// the seccomp filters, the address-family restrictions. Reproducing all of it here
// would mean shipping two versions of the hardening with no way to test the
// generated one from a Mac, and a subtle difference between them is worse than an
// honest gap. deploy/deployment.md says which is which.

// stopTimeoutSeconds is how long a service manager should wait after SIGTERM.
//
// Larger than the cache's own drain budget on purpose: the daemon should be the
// thing that decides it has run out of time, because it is the only thing that can
// then say what it did not finish.
const stopTimeoutSeconds = 300

// systemdUserUnit is the library's systemd template with TimeoutStopSec added.
//
// Everything else is verbatim from kardianos/service v1.2.2, including the pieces
// that look odd — EnvironmentFile pointing at /etc/sysconfig, the blank lines from
// the range blocks — because a faithful copy is reviewable against the original
// and an improved one is not.
const systemdUserUnit = `[Unit]
Description={{.Description}}
ConditionFileIsExecutable={{.Path|cmdEscape}}
{{range $i, $dep := .Dependencies}}
{{$dep}} {{end}}

[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart={{.Path|cmdEscape}}{{range .Arguments}} {{.|cmd}}{{end}}
{{if .ChRoot}}RootDirectory={{.ChRoot|cmd}}{{end}}
{{if .WorkingDirectory}}WorkingDirectory={{.WorkingDirectory|cmdEscape}}{{end}}
{{if .UserName}}User={{.UserName}}{{end}}
{{if .ReloadSignal}}ExecReload=/bin/kill -{{.ReloadSignal}} "$MAINPID"{{end}}
{{if .PIDFile}}PIDFile={{.PIDFile|cmd}}{{end}}
{{if and .LogOutput .HasOutputFileSupport -}}
StandardOutput=file:{{.LogDirectory}}/{{.Name}}.out
StandardError=file:{{.LogDirectory}}/{{.Name}}.err
{{- end}}
{{if gt .LimitNOFILE -1 }}LimitNOFILE={{.LimitNOFILE}}{{end}}
{{if .Restart}}Restart={{.Restart}}{{end}}
{{if .SuccessExitStatus}}SuccessExitStatus={{.SuccessExitStatus}}{{end}}
RestartSec=120
EnvironmentFile=-/etc/sysconfig/{{.Name}}

# Added by odctl: long enough for the caching gateway to finish uploading files
# OpenDrive does not have yet before systemd loses patience. systemd's own default
# is 90s; the daemon's drain budget is 2min, so the default would cut it short and
# kill the process before it could record what it had not finished.
TimeoutStopSec=` + stopTimeoutSecondsLiteral + `

{{range $k, $v := .EnvVars -}}
Environment={{$k}}={{$v}}
{{end -}}

[Install]
WantedBy=multi-user.target
`

// launchdAgent is the library's launchd template with ExitTimeOut added.
const launchdAgent = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Disabled</key>
	<false/>
	{{- if .EnvVars}}
	<key>EnvironmentVariables</key>
	<dict>
		{{- range $k, $v := .EnvVars}}
		<key>{{html $k}}</key>
		<string>{{html $v}}</string>
		{{- end}}
	</dict>
	{{- end}}
	<!-- Added by odctl: launchd kills after ExitTimeOut, which defaults to 20
	     seconds. The caching gateway needs longer than that to flush files
	     OpenDrive does not have yet, and being killed part-way through loses both
	     the upload and the record of what was unfinished. -->
	<key>ExitTimeOut</key>
	<integer>` + stopTimeoutSecondsLiteral + `</integer>
	<key>KeepAlive</key>
	<{{bool .KeepAlive}}/>
	<key>Label</key>
	<string>{{html .Name}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{html .Path}}</string>
		{{- if .Config.Arguments}}
		{{- range .Config.Arguments}}
		<string>{{html .}}</string>
		{{- end}}
	{{- end}}
	</array>
	{{- if .ChRoot}}
	<key>RootDirectory</key>
	<string>{{html .ChRoot}}</string>
	{{- end}}
	<key>RunAtLoad</key>
	<{{bool .RunAtLoad}}/>
	<key>SessionCreate</key>
	<{{bool .SessionCreate}}/>
	{{- if .StandardErrorPath}}
	<key>StandardErrorPath</key>
	<string>{{html .StandardErrorPath}}</string>
	{{- end}}
	{{- if .StandardOutPath}}
	<key>StandardOutPath</key>
	<string>{{html .StandardOutPath}}</string>
	{{- end}}
	{{- if .UserName}}
	<key>UserName</key>
	<string>{{html .UserName}}</string>
	{{- end}}
	{{- if .WorkingDirectory}}
	<key>WorkingDirectory</key>
	<string>{{html .WorkingDirectory}}</string>
	{{- end}}
</dict>
</plist>
`

// stopTimeoutSecondsLiteral is the same number as stopTimeoutSeconds, as a
// string, so the templates can be constants. A test asserts the two agree, which
// is cheaper than building the templates at run time for one substitution.
const stopTimeoutSecondsLiteral = "300"
