export interface Capability {
  name: string;
  available: boolean;
  detail?: string;
}

export interface Account {
  id: string;
  name: string;
  cloudflare_account_id: string;
  enabled: boolean;
  health_status: string;
  health_error?: string;
  capabilities?: Capability[];
  has_r2_credentials: boolean;
}

export interface R2Object {
  key: string;
  size: number;
  etag: string;
  content_type: string;
  last_modified: string;
  state: string;
  physical_bucket_id: string;
}

export type BucketLifecycleState = "active" | "deleting" | "delete_failed";

export interface Bucket {
  id: string;
  account_id: string;
  name: string;
  health_status: string;
  storage_bytes: number;
  reserved_storage_bytes: number;
  usage_checked_at: string;
  class_a_ops: number;
  class_b_ops: number;
  lifecycle_state?: BucketLifecycleState;
  deletion_job_id?: string;
}

export type BucketDeletionMode = "empty_only" | "empty_and_delete";

export interface BucketDeleteConfirmation {
  mode: BucketDeletionMode;
  confirmationName: string;
  adminPassword: string;
}

export interface BucketDeletionRequest {
  account_id: string;
  jurisdiction: string;
  mode: BucketDeletionMode;
  confirmation_name: string;
  admin_password: string;
}

export interface RemoteBucketView {
  name: string;
  jurisdiction?: string;
  creation_date?: string;
  payload_bytes?: number;
  metadata_bytes?: number;
  object_count?: number;
  managed: boolean;
  bucket_id?: string;
  health_status?: string;
  remote_missing?: boolean;
  lifecycle_state?: BucketLifecycleState;
  deletion_job_id?: string;
  deletion_status?: BackgroundJobStatus;
  deletion_error_code?: string;
  deletion_error?: string;
}

export interface R2AccountUsage {
  account_id: string;
  usage_month: string;
  managed_bytes: number;
  unmanaged_bytes: number;
  reserved_bytes: number;
  account_storage_soft_limit_bytes: number;
  class_a_ops: number;
  class_a_soft_limit: number;
  class_b_ops: number;
  class_b_soft_limit: number;
  usage_checked_at: string;
}

export interface Credential {
  id: string;
  kind: "s3" | "webdav" | "ai";
  name: string;
  public_id: string;
  scopes: string[];
  disabled: boolean;
  secret?: string;
}

export interface FileEntry {
  name: string;
  key: string;
  kind: "mount" | "directory" | "file";
  size: number;
  content_type: string;
  etag?: string;
  last_modified: string;
  mount_id?: string;
  disabled?: boolean;
}

export interface FileDirectoryList {
  path: string;
  entries: FileEntry[];
  directory_count: number;
  file_count: number;
  next_marker?: string;
  mount_id?: string;
  mount_name?: string;
}

export type BackgroundJobStatus = "pending" | "running" | "succeeded" | "failed";

export interface BucketDeletionJobPayload {
  account_id?: string;
  cloudflare_account_id?: string;
  local_bucket_id?: string;
  bucket_name?: string;
  jurisdiction?: string;
  expected_creation_date?: string;
  mode?: BucketDeletionMode;
  stage?: string;
  deleted_objects?: number;
  aborted_multipart?: number;
  remote_mutated?: boolean;
  remote_missing_at_enqueue?: boolean;
  delete_rounds?: number;
}

export interface BackgroundJob {
  id: string;
  type: string;
  resource_key?: string;
  parent_job_id?: string;
  status: BackgroundJobStatus;
  payload?: BucketDeletionJobPayload | Record<string, unknown> | string;
  progress: number;
  attempts?: number;
  max_attempts?: number;
  error?: string;
  error_code?: string;
  lease_until?: string;
  created_at?: string;
  updated_at?: string;
}
