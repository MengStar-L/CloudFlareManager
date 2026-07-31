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

export interface BackgroundJob {
  id: string;
  type: string;
  status: "pending" | "running" | "succeeded" | "failed";
  progress: number;
  error?: string;
}
