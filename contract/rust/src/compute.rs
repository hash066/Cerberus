//! Compute / inference contract types mirroring `proto/cerberus/v1/compute.proto`.

use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub enum ShardKind {
    #[default]
    Pipeline = 0,
    Tensor = 1,
    Data = 2,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Shard {
    pub kind: ShardKind,
    pub layer_lo: u32,
    pub layer_hi: u32,
    pub tp_rank: u32,
    pub tp_world: u32,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Promise {
    pub promise_id: Vec<u8>,
    pub producer: super::PeerId,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub enum TensorDType {
    #[default]
    Unspecified = 0,
    F32 = 1,
    F16 = 2,
    BF16 = 3,
    I8 = 4,
}

/// Compression on the activation data path. `FrontierZk` is a documented stub.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub enum CompressionHint {
    #[default]
    Unspecified = 0,
    None = 1,
    Zstd = 2,
    Lz4 = 3,
    /// Frontier: zk-compressed proof bundles — not implemented in v0.1.
    FrontierZk = 4,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ActivationFrame {
    pub payload: Vec<u8>,
    pub shape: Vec<u32>,
    pub dtype: TensorDType,
    pub compression: CompressionHint,
    pub stage_index: u32,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct PipelineStageMeta {
    pub stage_index: u32,
    pub shard: Shard,
    pub node: super::PeerId,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ComputeTask {
    pub task_id: Vec<u8>,
    pub component: Vec<u8>,
    pub shard: Shard,
    pub caps: Vec<Vec<u8>>,
    pub deps: Vec<Promise>,
    pub result_cap: Vec<u8>,
    pub activation: ActivationFrame,
    pub pipeline_stage: u32,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct InferenceTask {
    pub task: ComputeTask,
    pub activation: ActivationFrame,
    pub pipeline_stage: u32,
    pub stages: Vec<PipelineStageMeta>,
}

impl InferenceTask {
    pub fn to_compute_task(&self) -> ComputeTask {
        let mut t = self.task.clone();
        if !self.activation.payload.is_empty() || !self.activation.shape.is_empty() {
            t.activation = self.activation.clone();
        }
        if self.pipeline_stage != 0 {
            t.pipeline_stage = self.pipeline_stage;
        }
        t
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ComputeResult {
    pub task_id: Vec<u8>,
    pub ok: bool,
    pub output: Vec<u8>,
    pub error: String,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn inference_task_projection() {
        let it = InferenceTask {
            task: ComputeTask {
                task_id: b"tid".to_vec(),
                ..Default::default()
            },
            activation: ActivationFrame {
                payload: vec![0; 4],
                shape: vec![1],
                dtype: TensorDType::F32,
                compression: CompressionHint::None,
                ..Default::default()
            },
            pipeline_stage: 1,
            ..Default::default()
        };
        let ct = it.to_compute_task();
        assert_eq!(ct.pipeline_stage, 1);
        assert_eq!(ct.activation.payload.len(), 4);
    }

    #[test]
    fn activation_frame_serde_roundtrip() {
        let frame = ActivationFrame {
            payload: vec![1, 2, 3, 4],
            shape: vec![1],
            dtype: TensorDType::F32,
            compression: CompressionHint::None,
            stage_index: 2,
        };
        let json = serde_json::to_string(&frame).unwrap();
        let got: ActivationFrame = serde_json::from_str(&json).unwrap();
        assert_eq!(got, frame);
    }
}
