# frozen_string_literal: true

# Validates the aggregated check-in extension contract on its own.
#
# This script deliberately does not load control-api.openapi.yaml as a
# contract to validate: the extension is versioned separately and a Core build
# without it answers these paths with 404. The main contract is read only to
# assert the two documents stay separate.

require "json"
require "pathname"
require "yaml"

ROOT = Pathname.new(__dir__).realpath
CONTRACT_PATH = ROOT.join("checkin.openapi.yaml")
MAIN_CONTRACT_PATH = ROOT.parent.join("control-api.openapi.yaml")

def load_document(path)
  YAML.load_file(path.to_s)
end

def each_ref(value, &block)
  case value
  when Hash
    value.each do |key, child|
      block.call(child) if key == "$ref"
      each_ref(child, &block)
    end
  when Array
    value.each { |child| each_ref(child, &block) }
  end
end

def resolve_pointer(document, fragment, source)
  return document if fragment.empty?
  raise "invalid JSON pointer in #{source}: ##{fragment}" unless fragment.start_with?("/")

  fragment.split("/").drop(1).reduce(document) do |current, raw_token|
    token = raw_token.gsub("~1", "/").gsub("~0", "~")
    raise "$ref descends through a scalar at #{token.inspect} in #{source}" unless current.is_a?(Hash)
    raise "unresolved $ref token #{token.inspect} in #{source}" unless current.key?(token)

    current.fetch(token)
  end
end

contract = load_document(CONTRACT_PATH)
failures = []

def check(failures, condition, message)
  failures << message unless condition
end

# Every $ref resolves inside this document: the extension must not depend on
# the main contract's components, or changing one would silently change both.
reference_count = 0
each_ref(contract) do |reference|
  raise "non-string $ref" unless reference.is_a?(String)

  file_part, separator, fragment = reference.partition("#")
  check(failures, file_part.empty?, "extension $ref must stay in this document: #{reference}")
  next unless file_part.empty?

  resolve_pointer(contract, separator.empty? ? "" : fragment, reference)
  reference_count += 1
end
check(failures, reference_count.positive?, "extension contract contains no $ref values")

paths = contract.fetch("paths")
operations = {}
paths.each do |path, item|
  item.each do |method, operation|
    next unless %w[get post delete patch put].include?(method)

    operations["#{method.upcase} #{path}"] = operation
  end
end

# Separation from the core contract.
check(failures, contract.dig("info", "version") == "0.1.0", "extension version must be explicit")
paths.each_key do |path|
  check(failures, path.start_with?("/control/v1/extensions/checkin/"),
        "extension path escapes its namespace: #{path}")
end
main_contract = load_document(MAIN_CONTRACT_PATH)
overlap = main_contract.fetch("paths").keys & paths.keys
check(failures, overlap.empty?, "extension paths overlap the core contract: #{overlap.inspect}")

# Every operation is operator-only: an observer token must not reach check-in
# accounts, jobs or settings.
operations.each do |name, operation|
  check(failures, operation["x-astrlink-role"] == "operator",
        "#{name} must be operator-only, got #{operation['x-astrlink-role'].inspect}")
end

# Reads must be side-effect free, and the two routes that stay reachable while
# the extension is disabled must say so.
reads = operations.select { |name, _| name.start_with?("GET ") }
check(failures, reads.size >= 5, "expected several read operations, got #{reads.size}")
reads.each do |name, operation|
  check(failures, operation["x-astrlink-side-effects"] == "none",
        "#{name} must declare no side effects")
end
[
  "GET /control/v1/extensions/checkin/status",
  "GET /control/v1/extensions/checkin/settings",
].each do |name|
  check(failures, operations.fetch(name, {})["x-astrlink-available-when-disabled"] == true,
        "#{name} must remain available while the extension is disabled")
end
operations.each do |name, operation|
  next if name.end_with?("/status") || name.end_with?("/settings")

  check(failures, operation["x-astrlink-available-when-disabled"] != true,
        "#{name} must not be reachable while the extension is disabled")
end

# Writes are idempotent: a fixed request_id, a replay, and a conflict for the
# same id with different content. The Rust control transport retries some
# writes, so this is a contract requirement rather than a nicety.
writes = operations.reject { |name, _| name.start_with?("GET ") }
check(failures, writes.size >= 5, "expected several write operations, got #{writes.size}")
writes.each do |name, operation|
  next if name.start_with?("DELETE ")

  body_schema = operation.dig("requestBody", "content", "application/json", "schema")
  check(failures, !body_schema.nil?, "#{name} must declare a JSON request body")
  next if body_schema.nil?

  resolved = body_schema.key?("$ref") ? resolve_pointer(contract, body_schema.fetch("$ref").partition("#").last, name) : body_schema
  required = resolved.fetch("required", [])
  check(failures, required.include?("request_id"), "#{name} must require request_id")
  check(failures, resolved["additionalProperties"] == false, "#{name} body must reject unknown fields")
end
writes.each do |name, operation|
  codes = operation.fetch("responses").keys
  check(failures, codes.include?("409"), "#{name} must define 409 for a reused request_id with different content")
end

# Conditional writes report a revision conflict as 412, as the core contract
# does for a stale ETag.
[
  "PATCH /control/v1/extensions/checkin/accounts/{accountId}",
  "DELETE /control/v1/extensions/checkin/accounts/{accountId}",
  "POST /control/v1/extensions/checkin/authorizations",
].each do |name|
  codes = operations.fetch(name, {}).fetch("responses", {}).keys
  check(failures, codes.include?("412"), "#{name} must define 412 for expected_revision conflicts")
end

# Jobs are accepted asynchronously; a control request never waits for a site.
# A batch is the same operation with several accounts, so there is no separate
# batch path to keep in step.
[
  "POST /control/v1/extensions/checkin/jobs",
].each do |name|
  codes = operations.fetch(name, {}).fetch("responses", {}).keys
  check(failures, codes.include?("202"), "#{name} must answer 202 rather than waiting for the site")
end

schemas = contract.dig("components", "schemas")

# The documented job result set, including the states that exist because a
# dispatched submission cannot be retried blindly.
job_status = schemas.fetch("JobStatus").fetch("enum")
expected_status = %w[
  queued running success already_checked not_checked auth_required manual_required
  unsupported rate_limited retryable_failure uncertain cancelled
]
check(failures, job_status.sort == expected_status.sort,
      "job status set drifted: #{(job_status - expected_status) | (expected_status - job_status)}")

# The job is its own receipt: it must always say whether the submission left
# the machine, and where a reported result came from.
job = schemas.fetch("Job")
%w[dispatched proof_source].each do |field|
  check(failures, job.fetch("properties").key?(field), "Job must record #{field}")
  check(failures, job.fetch("required").include?(field), "Job must always state #{field}")
end

proof_source = schemas.fetch("ProofSource").fetch("enum")
check(failures, proof_source.sort == %w[none submission_response status_read].sort,
      "proof source set drifted: #{proof_source.inspect}")

# Pagination and body size are bounded by the contract, not by the handler.
list_limits = operations.select { |name, _| name.start_with?("GET ") }.filter_map do |name, operation|
  parameters = operation.fetch("parameters", []).map do |item|
    item.key?("$ref") ? resolve_pointer(contract, item.fetch("$ref").partition("#").last, name) : item
  end
  parameter = parameters.find { |item| item["name"] == "limit" }
  [name, parameter] if parameter
end
check(failures, list_limits.size >= 2, "list operations must declare a bounded limit")
list_limits.each do |name, parameter|
  schema = parameter.fetch("schema")
  check(failures, schema["maximum"] == 100 && schema["default"] == 20,
        "#{name} limit must match the bounded storage model (maximum 100, default 20)")
end
check(failures, schemas.dig("JobRequest", "properties", "accounts", "maxItems") == 25,
      "batch size must match MaxBatchAccounts (25)")

# No secret may appear in anything the extension publishes. Completing an
# authorization legitimately carries a session *inbound*, from the native
# bridge to Core, so that one request schema is excluded by name and checked
# separately: it must be write-only and must not be reachable from a response.
inbound_only = %w[AuthorizationCompletion]
forbidden = %w[cookie session session_token credential secret password access_token refresh_token]
published = schemas.reject { |name, _| inbound_only.include?(name) }
JSON.generate(published).scan(/"([a-z0-9_]+)":/).flatten.uniq.each do |field|
  check(failures, !forbidden.include?(field), "published schema exposes #{field}")
end

completion = schemas.fetch("AuthorizationCompletion")
check(failures, completion.fetch("properties").key?("credential"),
      "AuthorizationCompletion must be the single inbound credential carrier")
check(failures, completion.dig("properties", "credential", "writeOnly") == true,
      "the inbound credential must be marked writeOnly so it is never echoed")
response_bodies = operations.filter_map do |name, operation|
  refs = []
  each_ref(operation.fetch("responses", {})) { |reference| refs << reference }
  [name, refs] unless refs.empty?
end
response_bodies.each do |name, refs|
  inbound_only.each do |schema_name|
    check(failures, refs.none? { |reference| reference.end_with?("/#{schema_name}") },
          "#{name} returns the inbound-only schema #{schema_name}")
  end
end

if failures.empty?
  puts "checkin extension contract OK (#{operations.size} operations, #{reference_count} refs)"
else
  warn "checkin extension contract validation failed:"
  failures.each { |failure| warn "  - #{failure}" }
  exit 1
end
