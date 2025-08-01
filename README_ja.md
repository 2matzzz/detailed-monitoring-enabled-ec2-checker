# EC2 Detailed Monitoring Checker

AWS EC2インスタンスの詳細モニタリング設定を一括確認・無効化するCLIツールです。

## 機能

- 複数のAWSプロファイル・リージョンにわたってEC2インスタンスの詳細モニタリング状態をチェック
- 詳細モニタリングが有効なインスタンスの一括無効化
- Auto Scaling Group、Elastic Beanstalk環境との関連性を考慮した除外機能
- 結果のCSV出力
- プログレスバー表示による進捗確認
- 設定ファイルによるカスタマイズ
- リトライ機能とエラーハンドリング

## インストール

```bash
git clone https://github.com/2matzzz/detailed-monitoring-enabled-ec2-checker.git
cd detailed-monitoring-enabled-ec2-checker
go mod tidy
go build -o ec2-checker .
```

## 使用方法

### 基本的な使用方法

```bash
# 詳細モニタリングが有効なインスタンスをチェック
./ec2-checker check profile1 profile2

# 詳細モニタリングを無効化
./ec2-checker disable profile1 profile2

# 詳細ログで実行
./ec2-checker disable -verbose profile1 profile2

# ヘルプを表示
./ec2-checker -help
```

### オプション

- `-config <path>`: 設定ファイルのパスを指定
- `-dry-run`: 実際の変更を行わずに結果をプレビュー
- `-progress <true|false>`: プログレスバーの表示を制御（デフォルト: true）
- `-verbose`: 詳細なログを出力
- `-help`: ヘルプメッセージを表示

## 設定ファイル

`config.yaml`を作成することで動作をカスタマイズできます：

```yaml
# API設定
api_timeout: "30s"
total_timeout: "20m"
max_retries: 3
batch_size: 20
max_concurrency: 10

# 動作設定
dry_run: false
show_progress: true
verbose: false

# ログ設定
log_level: "info"  # debug, info, warn, error
log_format: "console"  # console, json

# 除外設定
exclude_asg_instances: true
exclude_eb_instances: true
```

### 環境変数

設定は環境変数でも指定可能です：

```bash
export EC2_CHECKER_API_TIMEOUT="30s"
export EC2_CHECKER_MAX_CONCURRENCY="10"
export EC2_CHECKER_DRY_RUN="true"
export EC2_CHECKER_LOG_LEVEL="debug"
```

## 出力ファイル

実行時に以下のCSVファイルが生成されます：

- `instances_with_detailed_monitoring.csv`: 詳細モニタリングが有効なEC2インスタンス
- `related_asgs.csv`: 関連するAuto Scaling Group
- `excluded_asg_instances.csv`: ASGにより除外されたインスタンス
- `excluded_eb_instances.csv`: Elastic Beanstalkにより除外されたインスタンス

## 動作の詳細

### インスタンス除外ロジック

以下の条件に該当するインスタンスは自動的に除外されます：

1. **Auto Scaling Group所属インスタンス** (`exclude_asg_instances: true`の場合)
   - ASGのLaunch TemplateまたはLaunch Configurationで詳細モニタリングが設定されている可能性があるため

2. **Elastic Beanstalk環境のインスタンス** (`exclude_eb_instances: true`の場合)
   - EB環境の設定で管理されているため

### エラーハンドリング

- AWS APIのレート制限に対する指数バックオフリトライ
- ページネーション処理での無限ループ防止
- ネットワークエラーや一時的な障害に対する自動リトライ
- プロファイル・リージョンレベルでのエラー分離

### パフォーマンス最適化

- 並行処理によるマルチリージョン・マルチプロファイル対応
- ページネーション対応による大量データの効率的処理
- セマフォによる同時接続数制御
- CSVストリーミング書き込みによるメモリ効率化

## 必要な権限

使用するAWSプロファイルには以下の権限が必要です：

```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "ec2:DescribeInstances",
                "ec2:DescribeRegions",
                "ec2:UnmonitorInstances",
                "autoscaling:DescribeAutoScalingGroups",
                "elasticbeanstalk:DescribeEnvironments",
                "sts:GetCallerIdentity"
            ],
            "Resource": "*"
        }
    ]
}
```

## トラブルシューティング

### よくある問題

1. **権限エラー**
   ```
   Unable to describe instances: operation error EC2: DescribeInstances, https response error StatusCode: 403
   ```
   → IAM権限を確認してください

2. **プロファイル設定エラー**
   ```
   Profile "profile-name": unable to describe regions
   ```
   → `~/.aws/config`と`~/.aws/credentials`の設定を確認してください

3. **処理が遅い**
   → `max_concurrency`を調整するか、`-verbose`でボトルネックを特定してください

### デバッグ

```bash
# 詳細ログで実行
./ec2-checker disable -verbose profile1

# 設定ファイルでデバッグレベルを設定
echo "log_level: debug" > config.yaml
./ec2-checker -config config.yaml disable profile1
```

## 注意事項

- 本ツールは詳細モニタリングを**無効化**します。コスト削減が目的ですが、モニタリング情報が失われることを理解した上で使用してください
- Auto Scaling GroupやElastic Beanstalk環境のインスタンスは自動的に除外されますが、必要に応じて設定で無効化できます
- 大量のインスタンスを処理する場合、AWS APIの制限により時間がかかる場合があります

## ライセンス

MIT License

## 貢献

Issue報告やPull Requestをお待ちしています。

