{
  description = "tturl";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
    go-overlay = {
      url = "github:purpleclay/go-overlay/v1.4.0";
      flake = false;
    };
  };

  outputs =
    { self, nixpkgs, go-overlay, ... }:
    let
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      pkgsFor = system:
        import nixpkgs {
          inherit system;
          overlays = [ (import go-overlay) ];
        };
      snapshotDateFor = source:
        let
          raw = source.lastModifiedDate or "";
          match = builtins.match "([0-9]{4})([0-9]{2})([0-9]{2})[0-9]*" raw;
        in
        if match == null then
          "unknown-date"
        else
          "${builtins.elemAt match 0}-${builtins.elemAt match 1}-${builtins.elemAt match 2}";
      baseVersion =
        let
          value = builtins.readFile ./release/version.txt;
          version = nixpkgs.lib.removeSuffix "\n" value;
        in
        assert value == "${version}\n";
        version;
      snapshotDate = snapshotDateFor self;
      packageVersion = "${baseVersion}-unstable-${snapshotDate}";
      stableBaseline =
        builtins.match "[0-9]+\\.[0-9]+\\.[0-9]+" baseVersion != null;
      nextPatchVersion =
        "${nixpkgs.lib.versions.majorMinor baseVersion}."
        + toString (nixpkgs.lib.toInt (nixpkgs.lib.versions.patch baseVersion) + 1);
      revision = self.rev or self.dirtyRev or "";
      modified = self ? dirtyRev;
      treeState =
        if revision == "" then
          "unknown"
        else if modified then
          "dirty"
        else
          "clean";
      revisionWithoutDirty = nixpkgs.lib.removeSuffix "-dirty" revision;
      shortRevision = builtins.substring 0 12 revisionWithoutDirty;
      diagnosticVersion =
        if revision == "" then
          "devel"
        else
          "devel (${shortRevision}${nixpkgs.lib.optionalString modified "-dirty"})";
      packageFor =
        system:
        let
          pkgs = pkgsFor system;
          source = pkgs.lib.fileset.toSource {
            root = ./.;
            fileset = pkgs.lib.fileset.unions [
              ./LICENSE
              ./THIRD_PARTY_LICENSES
              ./cmd/tturl
              ./go.mod
              ./go.sum
              ./govendor.toml
              ./internal
              ./release
              ./stats
              ./tth2
            ];
          };
        in
        pkgs.buildGoApplication {
          pname = "tturl";
          version = packageVersion;

          src = source;
          go = pkgs.go-bin.fromGoMod ./go.mod;
          modules = ./govendor.toml;

          subPackages = [ "cmd/tturl" ];

          nativeBuildInputs = [ pkgs.installShellFiles ];

          CGO_ENABLED = "0";
          ldflags = [
            "-X github.com/tantosec/tturl/internal/buildinfo.injectedRevision=${revisionWithoutDirty}"
            "-X github.com/tantosec/tturl/internal/buildinfo.injectedTree=${treeState}"
          ];

          postInstall = ''
            install -Dm644 LICENSE "$out/share/doc/tturl/LICENSE"
            install -Dm644 THIRD_PARTY_LICENSES "$out/share/doc/tturl/THIRD_PARTY_LICENSES"
            installShellCompletion --cmd tturl \
              --bash <($out/bin/tturl completion bash) \
              --zsh <($out/bin/tturl completion zsh) \
              --fish <($out/bin/tturl completion fish)
          '';

          passthru = {
            inherit diagnosticVersion packageVersion snapshotDate;
            sourceRevision = revisionWithoutDirty;
            sourceTreeState = treeState;
          };

          meta = {
            description = "HTTP/2 CLI for timeless timing attacks and request races";
            homepage = "https://github.com/tantosec/tturl";
            license = pkgs.lib.licenses.mit;
            mainProgram = "tturl";
          };
        };
    in
    {
      packages = forAllSystems (
        system:
        let
          tturl = packageFor system;
        in
        {
          inherit tturl;
        }
      );

      apps = forAllSystems (system: {
        tturl = {
          type = "app";
          program = "${self.packages.${system}.tturl}/bin/tturl";
          meta.description = "Run tturl";
        };
      });

      checks = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
          tturl = self.packages.${system}.tturl;
        in
        {
          install = pkgs.runCommand "tturl-install-check" {
            nativeBuildInputs = [ pkgs.jq ];
          } ''
            cmp ${./LICENSE} ${tturl}/share/doc/tturl/LICENSE
            cmp ${./THIRD_PARTY_LICENSES} ${tturl}/share/doc/tturl/THIRD_PARTY_LICENSES
            actual="$(${tturl}/bin/tturl --version)"
            expected="tturl ${tturl.diagnosticVersion}"
            if [ "$actual" != "$expected" ]; then
              echo "version output: $actual" >&2
              echo "expected:       $expected" >&2
              exit 1
            fi
            ${tturl}/bin/tturl --help >/dev/null

            report="$TMPDIR/report.jsonl"
            if ${tturl}/bin/tturl race --report json --trials 1 \
              'https://127.0.0.1:0/' >"$report" 2>/dev/null; then
              echo 'port-zero request unexpectedly succeeded' >&2
              exit 1
            fi

            if ! jq -e --arg revision '${tturl.sourceRevision}' \
              --arg tree '${tturl.sourceTreeState}' '
                select(.kind == "run") |
                .tool.version == "devel" and
                (.tool | has("revision") and has("modified")) and
                (if $revision == "" then
                   .tool.revision == null
                 else
                   .tool.revision == $revision
                 end) and
                (if $tree == "unknown" then
                   .tool.modified == null
                 else
                   .tool.modified == ($tree == "dirty")
                 end)
              ' "$report" >/dev/null; then
              echo 'JSON build identity check failed:' >&2
              cat "$report" >&2
              exit 1
            fi
            if ! jq -e '
              select(.kind == "request") |
              .headers["user-agent"] == ["tturl/devel"]
            ' "$report" >/dev/null; then
              echo 'JSON user-agent check failed:' >&2
              cat "$report" >&2
              exit 1
            fi
            touch "$out"
          '';
          metadata =
            assert snapshotDateFor { } == "unknown-date";
            assert snapshotDateFor { lastModifiedDate = "20260901123456"; } == "2026-09-01";
            assert tturl.version == tturl.packageVersion;
            assert !stableBaseline
              || builtins.compareVersions tturl.packageVersion baseVersion == 1;
            assert !stableBaseline
              || builtins.compareVersions tturl.packageVersion nextPatchVersion == -1;
            pkgs.runCommand "tturl-metadata-check" { } ''
              touch "$out"
            '';
          source = pkgs.runCommand "tturl-source-check" { } ''
            test -f ${tturl.src}/go.mod
            test -f ${tturl.src}/govendor.toml
            test -f ${tturl.src}/cmd/tturl/main.go
            test -f ${tturl.src}/release/version.txt
            touch "$out"
          '';
        }
      );
    };
}
