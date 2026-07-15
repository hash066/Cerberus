cmake_minimum_required(VERSION 3.16)
project(cerberus-llama-forward CXX)

set(CMAKE_CXX_STANDARD 17)

if(NOT DEFINED LLAMA_CPP_DIR)
    message(FATAL_ERROR "Set -DLLAMA_CPP_DIR=/path/to/llama.cpp")
endif()

add_executable(cerberus-llama-forward forward.cpp)
target_include_directories(cerberus-llama-forward PRIVATE "${LLAMA_CPP_DIR}/include")
target_link_directories(cerberus-llama-forward PRIVATE "${LLAMA_CPP_DIR}/build")
target_link_libraries(cerberus-llama-forward PRIVATE llama ggml)
